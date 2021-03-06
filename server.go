package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	nanoid "github.com/matoous/go-nanoid"
	"golang.org/x/net/netutil"
)

const (
	healthPath                         = "/health"
	contextDispositionFilenameFallback = "image"
)

var (
	mimes = map[imageType]string{
		imageTypeJPEG: "image/jpeg",
		imageTypePNG:  "image/png",
		imageTypeWEBP: "image/webp",
		imageTypeGIF:  "image/gif",
		imageTypeICO:  "image/x-icon",
	}

	contentDispositionsFmt = map[imageType]string{
		imageTypeJPEG: "inline; filename=\"%s.jpg\"",
		imageTypePNG:  "inline; filename=\"%s.png\"",
		imageTypeWEBP: "inline; filename=\"%s.webp\"",
		imageTypeGIF:  "inline; filename=\"%s.gif\"",
		imageTypeICO:  "inline; filename=\"%s.ico\"",
	}

	authHeaderMust []byte

	imgproxyIsRunningMsg = []byte("imgproxy is running")

	errInvalidMethod = newError(422, "Invalid request method", "Method doesn't allowed")
	errInvalidSecret = newError(403, "Invalid secret", "Forbidden")
)

var responseBufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

type httpHandler struct {
	sem chan struct{}
}

func newHTTPHandler() *httpHandler {
	return &httpHandler{make(chan struct{}, conf.Concurrency)}
}

func startServer() *http.Server {
	l, err := net.Listen("tcp", conf.Bind)
	if err != nil {
		log.Fatal(err)
	}
	s := &http.Server{
		Handler:        newHTTPHandler(),
		ReadTimeout:    time.Duration(conf.ReadTimeout) * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	go func() {
		log.Printf("Starting server at %s\n", conf.Bind)
		if err := s.Serve(netutil.LimitListener(l, conf.MaxClients)); err != nil && err != http.ErrServerClosed {
			log.Fatalln(err)
		}
	}()

	return s
}

func shutdownServer(s *http.Server) {
	log.Println("Shutting down the server...")

	ctx, close := context.WithTimeout(context.Background(), 5*time.Second)
	defer close()

	s.Shutdown(ctx)
}

func logResponse(status int, msg string) {
	var color int

	if status >= 500 {
		color = 31
	} else if status >= 400 {
		color = 33
	} else {
		color = 32
	}

	log.Printf("|\033[7;%dm %d \033[0m| %s\n", color, status, msg)
}

func writeCORS(rw http.ResponseWriter) {
	if len(conf.AllowOrigin) > 0 {
		rw.Header().Set("Access-Control-Allow-Origin", conf.AllowOrigin)
		rw.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONs")
	}
}

func contentDisposition(imageURL string, imgtype imageType) string {
	url, err := url.Parse(imageURL)
	if err != nil {
		return fmt.Sprintf(contentDispositionsFmt[imgtype], contextDispositionFilenameFallback)
	}

	_, filename := filepath.Split(url.Path)
	if len(filename) == 0 {
		return fmt.Sprintf(contentDispositionsFmt[imgtype], contextDispositionFilenameFallback)
	}

	return fmt.Sprintf(contentDispositionsFmt[imgtype], strings.TrimSuffix(filename, filepath.Ext(filename)))
}

func respondWithImage(ctx context.Context, reqID string, r *http.Request, rw http.ResponseWriter, data []byte) {
	po := getProcessingOptions(ctx)

	rw.Header().Set("Expires", time.Now().Add(time.Second*time.Duration(conf.TTL)).Format(http.TimeFormat))
	rw.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d, public", conf.TTL))
	rw.Header().Set("Content-Type", mimes[po.Format])
	rw.Header().Set("Content-Disposition", contentDisposition(getImageURL(ctx), po.Format))

	if conf.GZipCompression > 0 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		buf := responseBufPool.Get().(*bytes.Buffer)
		defer responseBufPool.Put(buf)

		buf.Reset()

		gzipData(data, buf)

		rw.Header().Set("Content-Encoding", "gzip")
		rw.Header().Set("Content-Length", strconv.Itoa(buf.Len()))

		rw.WriteHeader(200)
		buf.WriteTo(rw)
	} else {
		rw.Header().Set("Content-Length", strconv.Itoa(len(data)))
		rw.WriteHeader(200)
		rw.Write(data)
	}

	logResponse(200, fmt.Sprintf("[%s] Processed in %s: %s; %+v", reqID, getTimerSince(ctx), getImageURL(ctx), po))
}

func respondWithError(reqID string, rw http.ResponseWriter, err *imgproxyError) {
	logResponse(err.StatusCode, fmt.Sprintf("[%s] %s", reqID, err.Message))

	rw.WriteHeader(err.StatusCode)
	rw.Write([]byte(err.PublicMessage))
}

func respondWithOptions(reqID string, rw http.ResponseWriter) {
	logResponse(200, fmt.Sprintf("[%s] Respond with options", reqID))
	rw.WriteHeader(200)
}

func respondWithNotModified(reqID string, rw http.ResponseWriter) {
	logResponse(200, fmt.Sprintf("[%s] Not modified", reqID))
	rw.WriteHeader(304)
}

func prepareAuthHeaderMust() []byte {
	if len(authHeaderMust) == 0 {
		authHeaderMust = []byte(fmt.Sprintf("Bearer %s", conf.Secret))
	}

	return authHeaderMust
}

func checkSecret(r *http.Request) bool {
	if len(conf.Secret) == 0 {
		return true
	}

	return subtle.ConstantTimeCompare(
		[]byte(r.Header.Get("Authorization")),
		prepareAuthHeaderMust(),
	) == 1
}

func (h *httpHandler) lock() {
	h.sem <- struct{}{}
}

func (h *httpHandler) unlock() {
	<-h.sem
}

func (h *httpHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	reqID, _ := nanoid.Nanoid()

	defer func() {
		if rerr := recover(); rerr != nil {
			if err, ok := rerr.(error); ok {
				reportError(err, r)

				if ierr, ok := err.(*imgproxyError); ok {
					respondWithError(reqID, rw, ierr)
				} else {
					respondWithError(reqID, rw, newUnexpectedError(err, 4))
				}
			} else {
				panic(rerr)
			}
		}
	}()

	log.Printf("[%s] %s: %s\n", reqID, r.Method, r.URL.RequestURI())

	writeCORS(rw)

	if r.Method == http.MethodOptions {
		respondWithOptions(reqID, rw)
		return
	}

	if r.Method != http.MethodGet {
		panic(errInvalidMethod)
	}

	if !checkSecret(r) {
		panic(errInvalidSecret)
	}

	if r.URL.RequestURI() == healthPath {
		rw.WriteHeader(200)
		rw.Write(imgproxyIsRunningMsg)
		return
	}

	ctx := context.Background()

	if newRelicEnabled {
		var newRelicCancel context.CancelFunc
		ctx, newRelicCancel = startNewRelicTransaction(ctx, rw, r)
		defer newRelicCancel()
	}

	if prometheusEnabled {
		prometheusRequestsTotal.Inc()
		defer startPrometheusDuration(prometheusRequestDuration)()
	}

	h.lock()
	defer h.unlock()

	ctx, timeoutCancel := startTimer(ctx, time.Duration(conf.WriteTimeout)*time.Second)
	defer timeoutCancel()

	ctx, err := parsePath(ctx, r)
	if err != nil {
		panic(err)
	}

	ctx, downloadcancel, err := downloadImage(ctx)
	defer downloadcancel()
	if err != nil {
		if newRelicEnabled {
			sendErrorToNewRelic(ctx, err)
		}
		if prometheusEnabled {
			incrementPrometheusErrorsTotal("download")
		}
		panic(err)
	}

	checkTimeout(ctx)

	if conf.ETagEnabled {
		eTag := calcETag(ctx)
		rw.Header().Set("ETag", eTag)

		if eTag == r.Header.Get("If-None-Match") {
			respondWithNotModified(reqID, rw)
			return
		}
	}

	checkTimeout(ctx)

	imageData, err := processImage(ctx)
	if err != nil {
		if newRelicEnabled {
			sendErrorToNewRelic(ctx, err)
		}
		if prometheusEnabled {
			incrementPrometheusErrorsTotal("processing")
		}
		panic(err)
	}

	checkTimeout(ctx)

	respondWithImage(ctx, reqID, r, rw, imageData)
}
