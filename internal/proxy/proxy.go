package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"

	"github.com/t1d333/proxyhw/internal/repository"
	"go.uber.org/zap"
)

var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Connection",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

type ForwardProxy struct {
	logger       *zap.SugaredLogger
	rep          repository.Repository
	certificates sync.Map
}

func NewForwardProxy(logger *zap.SugaredLogger, rep repository.Repository) *ForwardProxy {
	return &ForwardProxy{logger, rep, sync.Map{}}
}

func (p *ForwardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.logger.Infow("new request", "url", r.URL.String(), "method", r.Method)
	if r.Method == http.MethodConnect {
		p.handleHTTPS(w, r)
	} else {
		p.handleHTTP(w, r)
	}
}

func (p *ForwardProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	r.RequestURI = ""
	for _, header := range hopHeaders {
		r.Header.Del(header)
	}

	body, _ := io.ReadAll(r.Body)
	tmp := r.Clone(context.TODO())
	tmp.Body = io.NopCloser(bytes.NewReader(body))
	r.Body = io.NopCloser(bytes.NewReader(body))

	response, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		p.logger.Error("failed to send request", zap.Error(err))
		return
	}

	if err := p.rep.CreateRequestResponsePair(tmp, response); err != nil {
		p.logger.Error("failed to save request", zap.Error(err))
	}

	defer response.Body.Close()

	for k, vv := range response.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		p.logger.Error("failed to copy data from response", zap.Error(err))
	}
}

func (p *ForwardProxy) handleHTTPS(w http.ResponseWriter, rawReq *http.Request) {
	w.WriteHeader(http.StatusOK)

	hj, ok := w.(http.Hijacker)
	if !ok {
		p.logger.Error("failed to convert hijacker")
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	conn, _, err := hj.Hijack()
	defer conn.Close()

	if err != nil {
		p.logger.Error("failed to hijack conn", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	host, _, err := net.SplitHostPort(rawReq.Host)
	if err != nil {
		p.logger.Error("failed to splitting host/port", zap.Error(err))
		return
	}

	if _, ok := p.certificates.Load(host); !ok {
		cert, err := p.createCert(host)
		if err != nil {
			p.logger.Error("failed to generate certificate", zap.Error(err), zap.String("host", host))
			return
		}

		p.certificates.Store(host, cert)
	}

	cert, _ := p.certificates.Load(host)
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert.(tls.Certificate)},
	}

	tlsConn := tls.Server(conn, tlsConfig)
	defer tlsConn.Close()

	reader := bufio.NewReader(tlsConn)

	r, err := http.ReadRequest(reader)
	if err != nil {
		if errors.Is(err, io.EOF) {
			p.logger.Error("failed to read request", zap.Error(err))
		}
		return
	}

	p.logger.Infow("new tls request", "url", r.URL.String(), "host", r.Host, "method", r.Method)

	p.updateURL(r, rawReq.Host)
	for _, h := range hopHeaders {
		r.Header.Del(h)
	}

	body, _ := io.ReadAll(r.Body)
	clone := r.Clone(context.TODO())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	r.Body = io.NopCloser(bytes.NewReader(body))

	response, err := http.DefaultClient.Do(r)
	if err != nil {
		p.logger.Error("failed to send request", zap.Error(err))
		return
	}

	if err := p.rep.CreateRequestResponsePair(clone, response); err != nil {
		p.logger.Error("failed to save request", zap.Error(err))
	}

	defer response.Body.Close()

	if err := response.Write(tlsConn); err != nil {
		p.logger.Error("failed to write response to tls connection", zap.Error(err))
	}
}

func (p *ForwardProxy) updateURL(r *http.Request, host string) {
	if !strings.HasPrefix(r.URL.String(), "https") {
		host = "https://" + host
	}

	newURL, err := url.Parse(host)
	if err != nil {
		p.logger.Error("failed to update request url", zap.Error(err))
	}

	newURL.Path = r.URL.Path
	newURL.RawQuery = r.URL.RawQuery

	r.URL = newURL
	r.RequestURI = ""
}

func (p *ForwardProxy) createCert(host string) (tls.Certificate, error) {
	cmd := exec.Command("/bin/sh", "/scripts/gen_cert.sh", host)

	if err := cmd.Run(); err != nil {
		p.logger.Error("failed to generate new cert", zap.Error(err))
	}

	return tls.LoadX509KeyPair(fmt.Sprintf("/certs/%s.crt", host), "/cert.key")
}
