package relay

import (
	"context"
	"io"
	"net"
	"net/http"
)

// connect carries opaque bytes to one configured TCP address. Neither the
// CONNECT authority nor any request header can change that address. TLS and
// repository authorization belong to the proxy and code host, not the relay.
func connect(w http.ResponseWriter, req *http.Request, address string) (int, string, error) {
	if req.ProtoMajor != 2 || req.Method != http.MethodConnect {
		return refuse(w, http.StatusMethodNotAllowed, "only HTTP/2 CONNECT is relayed for codehost"), "method", nil
	}
	if req.URL.Path != "" || req.URL.RawPath != "" || req.URL.RawQuery != "" || req.URL.ForceQuery {
		return refuse(w, http.StatusBadRequest, "CONNECT must have no path or query"), "path", nil
	}
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(req.Context(), "tcp", address)
	if err != nil {
		return refuse(w, http.StatusBadGateway, "upstream connection failed"), "upstream-error", err
	}
	defer conn.Close()
	// A reset stream, closed tunnel, or stopped Run ends the TCP connection,
	// including when its peer is silent. There is no independent stream lifetime.
	stop := context.AfterFunc(req.Context(), func() { _ = conn.Close() })
	defer stop()
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		return http.StatusOK, "truncated", err
	}
	// The successful headers must reach the proxy before we read its body:
	// it waits for them before sending a TLS ClientHello. Each response write
	// also flushes, so small TLS handshake records cannot sit in an HTTP buffer.
	uploaded := make(chan struct{})
	go func() {
		defer close(uploaded)
		_, err := io.Copy(conn, req.Body)
		if err == nil {
			// Dialing tcp yields a TCPConn. END_STREAM closes only the upload
			// half, so the code host can finish sending its response.
			err = conn.(*net.TCPConn).CloseWrite()
		}
		if err != nil {
			_ = conn.Close()
		}
	}()
	_, err = io.Copy(flushWriter{w}, conn)
	// Upstream EOF ends the stream. Unblock and join the upload before the
	// handler returns, even if the proxy has not sent END_STREAM.
	_ = conn.Close()
	_ = req.Body.Close()
	<-uploaded
	if err != nil {
		return http.StatusOK, "truncated", err
	}
	return http.StatusOK, "connect", nil
}

// flushWriter forwards each chunk at once instead of buffering a small TLS
// record until the next write. Only the CONNECT response uses it.
type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		err = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}
