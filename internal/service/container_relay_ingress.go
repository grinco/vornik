package service

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"

	"vornik.io/vornik/internal/config"
)

// buildRelayIngressTLSConfig builds the mTLS server config for the relay
// ingress: load the server keypair + a client-CA pool, and REQUIRE a client
// cert signed by that CA. This is the DMZ→job-tier trust seam.
func buildRelayIngressTLSConfig(cfg config.RelayIngressConfig) (*tls.Config, error) {
	srvCert, err := tls.LoadX509KeyPair(cfg.ServerCert, cfg.ServerKey)
	if err != nil {
		return nil, fmt.Errorf("relay ingress server keypair: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.ClientCA)
	if err != nil {
		return nil, fmt.Errorf("relay ingress client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("relay ingress client CA: no certs parsed from %s", cfg.ClientCA)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{srvCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// initRelayIngress constructs the second http.Server (relay ingress) when this
// node runs workers AND relay_ingress is configured. Returns (nil, nil) when
// not applicable. Mounts ONLY the relay route — no other routes leak onto the
// mTLS listener.
func (c *Container) initRelayIngress() (*http.Server, error) {
	if !c.capabilities().RunWorkers {
		return nil, nil
	}
	ri := c.Config.Node.RelayIngress
	if ri.Addr == "" {
		// Validation guarantees all-or-nothing; empty Addr means not configured.
		return nil, nil
	}
	if c.apiServer == nil {
		return nil, fmt.Errorf("relay ingress: apiServer is nil (initHTTPServer must run first)")
	}
	tlsCfg, err := buildRelayIngressTLSConfig(ri)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/v1/webhook-relay", c.apiServer.RelayWebhook)
	mux.HandleFunc("/internal/v1/node-heartbeat", c.apiServer.NodeHeartbeat)
	return &http.Server{
		Addr:      ri.Addr,
		Handler:   mux,
		TLSConfig: tlsCfg,
		// ReadHeaderTimeout only: this is a single short-lived internal mTLS POST
		// from the DMZ relay node; the handler context bounds the rest of the
		// request lifetime. The fuller timeout set on the main HTTP server (read,
		// write, idle) is unnecessary here because the client is trusted,
		// connection-count is minimal, and long-running handler waits are capped
		// by the per-request context that RelayWebhook inherits.
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}
