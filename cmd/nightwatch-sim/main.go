// Command nightwatch-sim runs the TrueNAS and/or AMT protocol simulators on
// fixed ports so the integration tests can be driven against them in containers
// (docker-compose.test.yml) instead of in-process httptest servers. It is a thin
// wrapper over internal/sim; the wire behavior lives there and is shared with the
// in-process tests.
package main

import (
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/imlach/nightwatch/internal/sim"
)

func main() {
	which := flag.String("sim", envDefault("NIGHTWATCH_SIM", "truenas"), "which sim to run: truenas | amt")
	addr := flag.String("addr", envDefault("NIGHTWATCH_SIM_ADDR", ":8443"), "listen address")
	apiKey := flag.String("api-key", os.Getenv("NIGHTWATCH_SIM_API_KEY"), "truenas: expected API key (empty accepts any)")
	realm := flag.String("realm", os.Getenv("NIGHTWATCH_SIM_REALM"), "amt: HTTP digest realm (empty disables auth)")
	on := flag.Bool("on", true, "amt: initial power state")
	flag.Parse()

	switch *which {
	case "truenas":
		runTrueNAS(*addr, *apiKey)
	case "amt":
		runAMT(*addr, *on, *realm)
	default:
		log.Fatalf("unknown -sim %q (want truenas|amt)", *which)
	}
}

// The internal/sim servers bind to an ephemeral port (httptest). For the
// container path we serve the same handlers on a fixed addr via ListenAndServe.

func runTrueNAS(addr, apiKey string) {
	srv := sim.NewTrueNASServer(apiKey)
	log.Printf("nightwatch-sim truenas listening on %s (wss://.../api/current)", addr)
	waitOrServeTLS(srv.HTTPServer(addr), srv.TLSCertPEM(), srv.TLSKeyPEM())
}

func runAMT(addr string, on bool, realm string) {
	srv := sim.NewAMTServer(on, realm)
	log.Printf("nightwatch-sim amt listening on %s (http://.../wsman)", addr)
	waitOrServe(srv.HTTPServer(addr))
}

func waitOrServe(srv *http.Server) {
	go shutdownOnSignal(srv)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func waitOrServeTLS(srv *http.Server, certPEM, keyPEM []byte) {
	go shutdownOnSignal(srv)
	if err := listenAndServeTLSPEM(srv, certPEM, keyPEM); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func shutdownOnSignal(srv *http.Server) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	_ = srv.Close()
}

// listenAndServeTLSPEM serves over TLS using in-memory PEM material (the sim's
// ephemeral self-signed cert) rather than files on disk.
func listenAndServeTLSPEM(srv *http.Server, certPEM, keyPEM []byte) error {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	return srv.ListenAndServeTLS("", "")
}

func envDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
