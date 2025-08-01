/*
 * Copyright Octelium Labs, LLC. All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License version 3,
 * as published by the Free Software Foundation of the License.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package httpg

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sigv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/octelium/octelium/apis/main/corev1"
	"github.com/octelium/octelium/cluster/vigil/vigil/modes/httpg/middlewares"
	"github.com/octelium/octelium/pkg/apiutils/ucorev1"
	"go.uber.org/zap"
	"golang.org/x/net/http/httpguts"
)

type directResponseHandler struct {
	direct *corev1.Service_Spec_Config_HTTP_Response_Direct
}

func (h *directResponseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	resp := h.direct
	switch resp.Type.(type) {
	case *corev1.Service_Spec_Config_HTTP_Response_Direct_Inline:
		w.Write([]byte(resp.GetInline()))
	case *corev1.Service_Spec_Config_HTTP_Response_Direct_InlineBytes:
		w.Write(resp.GetInlineBytes())
	default:
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 599 {
		w.WriteHeader(int(resp.StatusCode))
	}

	w.Header().Set("Server", "octelium")
}

func (s *Server) getProxy(ctx context.Context) (http.Handler, error) {
	reqCtx := middlewares.GetCtxRequestContext(ctx)

	isManagedSvc := ucorev1.ToService(reqCtx.Service).IsManagedService()

	cfg := reqCtx.ServiceConfig
	if cfg != nil && cfg.GetHttp() != nil && cfg.GetHttp().Response != nil && cfg.GetHttp().Response.GetDirect() != nil {
		return &directResponseHandler{
			direct: cfg.GetHttp().Response.GetDirect(),
		}, nil
	}

	upstream, err := s.lbManager.GetUpstream(ctx, reqCtx.AuthResponse)
	if err != nil {
		return nil, err
	}

	roundTripper, err := s.getRoundTripper(upstream)
	if err != nil {
		return nil, err
	}

	ret := &httputil.ReverseProxy{
		BufferPool: newBufferPool(),
		Transport:  roundTripper,
		Director: func(outReq *http.Request) {
			svc := reqCtx.Service

			switch upstream.URL.Scheme {
			case "https", "http":
				outReq.URL.Scheme = upstream.URL.Scheme
			case "ws":
				outReq.URL.Scheme = "http"
			case "grpc", "h2c":
				outReq.URL.Scheme = "http"
			case "wss":
				outReq.URL.Scheme = "https"
			default:
				if cfg != nil && (cfg.ClientCertificate != nil ||
					(cfg.Tls != nil && cfg.Tls.ClientCertificate != nil)) {
					outReq.URL.Scheme = "https"
				} else {
					outReq.URL.Scheme = "http"
				}
			}

			outReq.Host = upstream.URL.Host

			if upstream.IsUser {
				outReq.URL.Host = upstream.HostPort
			} else {
				outReq.URL.Host = upstream.URL.Host
			}

			outReq.URL.RawQuery = strings.ReplaceAll(outReq.URL.RawQuery, ";", "&")
			outReq.RequestURI = ""

			if _, ok := outReq.Header["User-Agent"]; !ok {
				outReq.Header.Set("User-Agent", "octelium")
			}

			// removeOcteliumCookie(outReq)
			fixWebSocketHeaders(outReq)

			if isHTTP2RequestUpstream(outReq, svc) {
				outReq.Proto = "HTTP/2"
				outReq.ProtoMajor = 2
				outReq.ProtoMinor = 0
			} else {
				outReq.Proto = "HTTP/1.1"
				outReq.ProtoMajor = 1
				outReq.ProtoMinor = 1
			}

			if !isManagedSvc {
				outReq.Header.Del("Forwarded")
				outReq.Header.Del("X-Forwarded-For")
				outReq.Header.Del("X-Forwarded-Host")
				outReq.Header.Del("X-Forwarded-Proto")
			}

			if outReq.Header.Get("Origin") != "" {
				outReq.Header.Set("Origin", upstream.URL.String())
			}

			if cfg != nil &&
				cfg.GetHttp() != nil && cfg.GetHttp().GetAuth() != nil &&
				cfg.GetHttp().GetAuth().GetSigv4() != nil {

				sigv4Opts := cfg.GetHttp().GetAuth().GetSigv4()
				secret, err := s.secretMan.GetByName(ctx, sigv4Opts.GetSecretAccessKey().GetFromSecret())
				if err == nil {
					signer := sigv4.NewSigner()
					if err := signer.SignHTTP(ctx,
						aws.Credentials{
							AccessKeyID:     sigv4Opts.AccessKeyID,
							SecretAccessKey: ucorev1.ToSecret(secret).GetValueStr(),
						},
						outReq,
						fmt.Sprintf("%x", sha256.Sum256([]byte(reqCtx.Body))),
						sigv4Opts.Service, sigv4Opts.Region,
						time.Now(),
					); err != nil {
						zap.L().Warn("Could not signHTTP for sigv4", zap.Error(err))
						return
					}
				} else {
					zap.L().Warn("Could not get sigv4 Secret", zap.Error(err))
				}
			}
		},

		FlushInterval: time.Duration(100 * time.Millisecond),
		ModifyResponse: func(r *http.Response) error {
			r.Header.Set("Server", "octelium")
			return nil
		},

		ErrorHandler: func(w http.ResponseWriter, request *http.Request, err error) {
			statusCode := http.StatusInternalServerError
			zap.S().Debugf("Handling response err: %+v", err)
			switch {
			case errors.Is(err, io.EOF):
				statusCode = http.StatusBadGateway
			case errors.Is(err, context.Canceled):
				statusCode = 499
			default:
				var netErr net.Error
				if errors.As(err, &netErr) {
					if netErr.Timeout() {
						statusCode = http.StatusGatewayTimeout
					} else {
						statusCode = http.StatusBadGateway
					}
				}
			}

			w.WriteHeader(statusCode)
			w.Write([]byte(http.StatusText(statusCode)))
		},
	}
	return ret, nil
}

/*
func removeOcteliumCookie(req *http.Request) {




	var cookieHdr string
	for _, cookie := range req.Cookies() {
		switch cookie.Name {
		case "octelium_auth", "octelium_rt":
			continue
		}
		if cookieHdr == "" {
			cookieHdr = fmt.Sprintf("%s=%s", cookie.Name, cookie.Value)
		} else {
			cookieHdr = fmt.Sprintf("%s; %s=%s", cookieHdr, cookie.Name, cookie.Value)
		}
	}

	req.Header.Set("Cookie", cookieHdr)
}
*/

func isWebSocketUpgrade(req *http.Request) bool {
	if !httpguts.HeaderValuesContainsToken(req.Header["Connection"], "Upgrade") {
		return false
	}

	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
}

func fixWebSocketHeaders(outReq *http.Request) {
	if !isWebSocketUpgrade(outReq) {
		return
	}

	outReq.Header["Sec-WebSocket-Key"] = outReq.Header["Sec-Websocket-Key"]
	outReq.Header["Sec-WebSocket-Extensions"] = outReq.Header["Sec-Websocket-Extensions"]
	outReq.Header["Sec-WebSocket-Accept"] = outReq.Header["Sec-Websocket-Accept"]
	outReq.Header["Sec-WebSocket-Protocol"] = outReq.Header["Sec-Websocket-Protocol"]
	outReq.Header["Sec-WebSocket-Version"] = outReq.Header["Sec-Websocket-Version"]
	delete(outReq.Header, "Sec-Websocket-Key")
	delete(outReq.Header, "Sec-Websocket-Extensions")
	delete(outReq.Header, "Sec-Websocket-Accept")
	delete(outReq.Header, "Sec-Websocket-Protocol")
	delete(outReq.Header, "Sec-Websocket-Version")
}
