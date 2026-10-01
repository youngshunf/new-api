package channeltests

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type RequestSender func(*gin.Context, *http.Request, *relaycommon.RelayInfo) (*http.Response, error)
type HeaderResolver func(*relaycommon.RelayInfo, *gin.Context) (map[string]string, error)

type wireObservation struct {
	method  string
	path    string
	headers http.Header
	body    []byte
}

// faultWire 只说真实HTTP协议并注入终端故障，不产生模型响应或业务成功数据。
type faultWire struct {
	listener       net.Listener
	protocol       string
	fault          string
	mu             sync.Mutex
	conns          map[net.Conn]struct{}
	warmGoAwaySent atomic.Bool
	requests       []*wireObservation
	errors         []error
	wg             sync.WaitGroup
}

func newFaultWire(t *testing.T, protocol, fault string) *faultWire {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	if protocol == "h2" {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		template := &x509.Certificate{
			SerialNumber: big.NewInt(12), Subject: pkix.Name{CommonName: "协议故障量具"},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			IsCA:        true, BasicConstraintsValid: true,
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		require.NoError(t, err)
		cert, err := x509.ParseCertificate(der)
		require.NoError(t, err)
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		previousEnabled, previousConfig := common.TLSInsecureSkipVerify, common.InsecureTLSConfig
		// 只为量具信任本次自签证书，仍校验TLS证书与主机名。
		common.TLSInsecureSkipVerify = true
		common.InsecureTLSConfig = &tls.Config{RootCAs: pool}
		t.Cleanup(func() {
			common.TLSInsecureSkipVerify, common.InsecureTLSConfig = previousEnabled, previousConfig
			service.ResetProxyClientCache()
		})
		ln = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
			NextProtos:   []string{"h2"},
		})
	}
	s := &faultWire{listener: ln, protocol: protocol, fault: fault, conns: make(map[net.Conn]struct{})}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.stop)
	service.InitHttpClient()
	t.Cleanup(service.ResetProxyClientCache)
	return s
}

func (s *faultWire) target() string {
	if s.protocol == "h2" {
		return "https://" + s.listener.Addr().String()
	}
	return "http://" + s.listener.Addr().String()
}

func (s *faultWire) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if s.protocol == "h2" {
				s.readHTTP2(conn)
			} else {
				s.readHTTP1(conn)
			}
		}()
	}
}

func (s *faultWire) record(request *wireObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
}

func (s *faultWire) recordError(err error) {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, err)
}

func (s *faultWire) readHTTP1(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			s.recordError(err)
			return
		}
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			s.recordError(err)
			return
		}
		if req.Method == http.MethodHead && req.URL.Path == "/warm" {
			// 204仅为连接预热，不是模型调用成功；后续完整POST到达后直接断开。
			_, err = io.WriteString(conn, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
			if err != nil {
				s.recordError(err)
				return
			}
			continue
		}
		s.record(&wireObservation{method: req.Method, headers: req.Header.Clone(), body: body})
		if s.fault == "307" || s.fault == "308" {
			_, err = fmt.Fprintf(conn, "HTTP/1.1 %s Redirect\r\nLocation: %s/redirected\r\nContent-Length: 0\r\n\r\n", s.fault, s.target())
			s.recordError(err)
			continue
		}
		return
	}
}

func (s *faultWire) readHTTP2(conn net.Conn) {
	tlsConn := conn.(*tls.Conn)
	if err := tlsConn.Handshake(); err != nil {
		s.recordError(err)
		return
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		s.recordError(errors.New("量具未协商HTTP2"))
		return
	}
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil {
		s.recordError(err)
		return
	}
	if string(preface) != http2.ClientPreface {
		s.recordError(errors.New("HTTP2前言不匹配"))
		return
	}
	framer := http2.NewFramer(conn, conn)
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := framer.WriteSettings(); err != nil {
		s.recordError(err)
		return
	}
	streams := make(map[uint32]*wireObservation)
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			s.recordError(err)
			return
		}
		var streamID uint32
		ended := false
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := framer.WriteSettingsAck(); err != nil {
					s.recordError(err)
					return
				}
			}
		case *http2.MetaHeadersFrame:
			request := &wireObservation{headers: make(http.Header)}
			for _, field := range f.Fields {
				if field.Name == ":method" {
					request.method = field.Value
				} else if field.Name == ":path" {
					request.path = field.Value
				} else if !strings.HasPrefix(field.Name, ":") {
					request.headers.Add(field.Name, field.Value)
				}
			}
			streamID, ended = f.StreamID, f.StreamEnded()
			streams[streamID] = request
			// 以实际HEADERS计执行请求数，不等body结束才记数；预热只验证连接，不是音频执行。
			if request.path != "/warm" {
				s.record(request)
			}
		case *http2.DataFrame:
			streamID, ended = f.StreamID, f.StreamEnded()
			if request := streams[streamID]; request != nil {
				request.body = append(request.body, f.Data()...)
			}
		}
		if !ended {
			continue
		}
		if request := streams[streamID]; request != nil && request.path == "/warm" {
			var block bytes.Buffer
			encoder := hpack.NewEncoder(&block)
			if err := encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "204"}); err != nil {
				s.recordError(err)
				return
			}
			if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: streamID, BlockFragment: block.Bytes(), EndHeaders: true, EndStream: true}); err != nil {
				s.recordError(err)
				return
			}
			if s.fault == "goaway_before_audio" && s.warmGoAwaySent.CompareAndSwap(false, true) {
				if err := framer.WriteGoAway(0, http2.ErrCodeNo, nil); err != nil {
					s.recordError(err)
				}
				return
			}
			continue
		}
		s.mu.Lock()
		requestCount := len(s.requests)
		s.mu.Unlock()
		if requestCount >= 2 {
			// 反例已在真实wire出现第二次提交，立即关闭量具形成确定失败，避免GoAway忙循环。
			_ = s.listener.Close()
			return
		}
		switch s.fault {
		case "goaway":
			s.recordError(framer.WriteGoAway(0, http2.ErrCodeNo, nil))
			return
		case "protocol_error":
			s.recordError(framer.WriteRSTStream(streamID, http2.ErrCodeProtocol))
		default:
			s.recordError(framer.WriteRSTStream(streamID, http2.ErrCodeRefusedStream))
		}
	}
}

func (s *faultWire) stop() {
	_ = s.listener.Close()
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *faultWire) observations(t *testing.T) []*wireObservation {
	t.Helper()
	s.stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Empty(t, s.errors, "协议量具本身不得失败或超时")
	return s.requests
}

func prepareRequest(t *testing.T, s *faultWire, mode int, resolve HeaderResolver) (*gin.Context, *http.Request, *relaycommon.RelayInfo, []byte) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	info := &relaycommon.RelayInfo{
		RelayMode: mode,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelSetting: dto.ChannelSettings{},
		},
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]interface{}{
			"Idempotency-Key":   "audio-key",
			"X-Idempotency-Key": "audio-key-2",
			"Content-Type":      "application/octet-stream",
		},
	}
	if s.protocol == "h1" {
		info.ChannelSetting.HTTPProtocol = dto.HTTPProtocolHTTP1
	}
	payload := []byte("single audio request bytes")
	var body io.Reader = bytes.NewReader(payload)
	if mode == relayconstant.RelayModeAudioTranscription || mode == relayconstant.RelayModeAudioTranslation {
		body = bytes.NewBuffer(payload)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.target()+"/audio", body)
	require.NoError(t, err)
	require.NotNil(t, req.GetBody, "真实标准requestBuilder会为Reader/Buffer创建GetBody")
	headers, err := resolve(info, c)
	require.NoError(t, err)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	// 还覆盖直接写入Header map的非规范键，不依赖CanonicalHeaderKey输入约束。
	req.Header["IDEMPOTENCY-KEY"] = []string{"noncanonical-key"}
	req.Header["x-idempotency-key"] = []string{"noncanonical-key-2"}
	return c, req, info, payload
}

// VerifyAudioTransportSingleSend 使用实际production client与远端协议故障计数，拒绝只验证GetBody标志。
func VerifyAudioTransportSingleSend(t *testing.T, send RequestSender, resolve HeaderResolver) {
	t.Helper()
	require.NotNil(t, send)
	require.NotNil(t, resolve)
	for _, mode := range []struct {
		name string
		mode int
	}{
		{name: "speech", mode: relayconstant.RelayModeAudioSpeech},
		{name: "transcription", mode: relayconstant.RelayModeAudioTranscription},
		{name: "translation", mode: relayconstant.RelayModeAudioTranslation},
	} {
		for _, fault := range []struct{ protocol, name string }{
			{protocol: "h1", name: "reused_connection_eof"},
			{protocol: "h2", name: "refused_stream"},
			{protocol: "h2", name: "goaway"},
			{protocol: "h2", name: "protocol_error"},
		} {
			t.Run(mode.name+"/"+fault.name, func(t *testing.T) {
				s := newFaultWire(t, fault.protocol, fault.name)
				c, req, info, payload := prepareRequest(t, s, mode.mode, resolve)
				if fault.protocol == "h1" {
					client, err := service.GetHttpClientWithProxySettings("", info.ChannelSetting)
					require.NoError(t, err)
					warm, err := client.Head(s.target() + "/warm")
					require.NoError(t, err)
					require.Equal(t, http.StatusNoContent, warm.StatusCode)
					require.NoError(t, warm.Body.Close())
				}
				var reused atomic.Bool
				req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
					GotConn: func(conn httptrace.GotConnInfo) {
						if conn.Reused {
							reused.Store(true)
						}
					},
				}))
				resp, err := send(c, req, info)
				require.Error(t, err, "协议故障必须显式返回终端错误")
				if fault.protocol == "h1" {
					assert.True(t, reused.Load(), "H1反例必须实际复用预热连接")
				}
				assert.NoError(t, req.Context().Err(), "终端必须来自协议故障，而非量具deadline")
				assert.Nil(t, resp)
				observations := s.observations(t)
				require.Equal(t, 1, len(observations), "实际远端请求/HEADERS必须只有一次")
				assert.Equal(t, http.MethodPost, observations[0].method)
				assert.Equal(t, payload, observations[0].body)
				assert.Equal(t, "application/octet-stream", observations[0].headers.Get("Content-Type"))
				assert.Empty(t, observations[0].headers.Values("Idempotency-Key"))
				assert.Empty(t, observations[0].headers.Values("X-Idempotency-Key"))
				assert.EqualValues(t, len(payload), req.ContentLength)
				assert.Nil(t, req.GetBody)
				assert.Equal(t, retrycontrol.DispatchStateUnknown, retrycontrol.ResolveDispatchState(c, false), "已写请求但未收到响应不得回落未派发")
			})
		}
	}
}

// VerifyAudioPreWriteConnectionReselection 验证连接失效时允许发送前重选，但音频HEADERS仍至多一次。
func VerifyAudioPreWriteConnectionReselection(t *testing.T, send RequestSender, resolve HeaderResolver) {
	t.Helper()
	s := newFaultWire(t, "h2", "goaway_before_audio")
	c, req, info, payload := prepareRequest(t, s, relayconstant.RelayModeAudioSpeech, resolve)
	client, err := service.GetHttpClientWithProxySettings("", info.ChannelSetting)
	require.NoError(t, err)
	warm, err := client.Head(s.target() + "/warm")
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, warm.StatusCode)
	require.NoError(t, warm.Body.Close())

	var selected atomic.Int32
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(conn httptrace.GotConnInfo) {
			selected.Add(1)
		},
	}))
	resp, err := send(c, req, info)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.True(t, s.warmGoAwaySent.Load(), "原连接必须在音频发送前收到真实GoAway")
	assert.GreaterOrEqual(t, selected.Load(), int32(1))
	observations := s.observations(t)
	assert.Equal(t, 2, len(s.conns), "预热连接GoAway后必须真实建立新连接")
	require.Equal(t, 1, len(observations), "重选不能变成第二个远端音频HEADERS")
	assert.Equal(t, payload, observations[0].body)
	assert.NoError(t, req.Context().Err())
}

// VerifyAudioRedirectNotFollowed 从真实三音频入口保留307/308响应，远端重定向目标不得再收到请求。
func VerifyAudioRedirectNotFollowed(t *testing.T, send RequestSender, resolve HeaderResolver) {
	t.Helper()
	for _, mode := range []int{relayconstant.RelayModeAudioSpeech, relayconstant.RelayModeAudioTranscription, relayconstant.RelayModeAudioTranslation} {
		for _, status := range []string{"307", "308"} {
			t.Run(fmt.Sprintf("mode_%d/%s", mode, status), func(t *testing.T) {
				s := newFaultWire(t, "h1", status)
				c, req, info, payload := prepareRequest(t, s, mode, resolve)
				resp, err := send(c, req, info)
				require.NoError(t, err)
				require.NotNil(t, resp)
				require.NoError(t, resp.Body.Close())
				assert.Equal(t, status, fmt.Sprint(resp.StatusCode))
				assert.Equal(t, s.target()+"/redirected", resp.Header.Get("Location"))
				observations := s.observations(t)
				require.Equal(t, 1, len(observations))
				assert.Equal(t, payload, observations[0].body)
				assert.Equal(t, retrycontrol.DispatchStateDispatched, retrycontrol.ResolveDispatchState(c, false))
			})
		}
	}
}

// VerifyAudioEmptyBodyRejected 保护nil/NoBody分支，不能用空流绕过transport不可重放语义。
func VerifyAudioEmptyBodyRejected(t *testing.T, send RequestSender) {
	t.Helper()
	for _, mode := range []int{relayconstant.RelayModeAudioSpeech, relayconstant.RelayModeAudioTranscription, relayconstant.RelayModeAudioTranslation} {
		for _, empty := range []struct {
			name string
			body io.ReadCloser
		}{{name: "nil"}, {name: "NoBody", body: http.NoBody}} {
			t.Run(fmt.Sprintf("mode_%d/%s", mode, empty.name), func(t *testing.T) {
				s := newFaultWire(t, "h1", "eof")
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
				req, err := http.NewRequest(http.MethodPost, s.target()+"/audio", nil)
				require.NoError(t, err)
				req.Body = empty.body
				info := &relaycommon.RelayInfo{RelayMode: mode, ChannelMeta: &relaycommon.ChannelMeta{}}
				resp, err := send(c, req, info)
				require.Error(t, err)
				assert.Nil(t, resp)
				var apiErr *types.NewAPIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, types.ErrorCodeInvalidRequest, apiErr.GetErrorCode())
				assert.Empty(t, s.observations(t), "非法空音频请求不得出现远端HEADERS")
			})
		}
	}
}

// VerifyNonAudioTransportRetryUnchanged 同一真实故障量具必须仍观察到非音频原transport重试，证明量具能证伪。
func VerifyNonAudioTransportRetryUnchanged(t *testing.T, send RequestSender, resolve HeaderResolver) {
	t.Helper()
	for _, protocol := range []string{"h1", "h2"} {
		t.Run(protocol, func(t *testing.T) {
			s := newFaultWire(t, protocol, "refused_stream")
			c, req, info, payload := prepareRequest(t, s, relayconstant.RelayModeChatCompletions, resolve)
			if protocol == "h1" {
				client, err := service.GetHttpClientWithProxySettings("", info.ChannelSetting)
				require.NoError(t, err)
				warm, err := client.Head(s.target() + "/warm")
				require.NoError(t, err)
				require.NoError(t, warm.Body.Close())
			}
			resp, err := send(c, req, info)
			require.Error(t, err)
			assert.Nil(t, resp)
			observations := s.observations(t)
			require.GreaterOrEqual(t, len(observations), 2, "同一量具必须真实捕获非音频重发，不能自己伪造计数")
			assert.NotNil(t, req.GetBody)
			assert.Equal(t, payload, observations[0].body)
			assert.Equal(t, payload, observations[1].body)
			assert.Contains(t, observations[0].headers.Values("Idempotency-Key"), "audio-key")
		})
	}
}
