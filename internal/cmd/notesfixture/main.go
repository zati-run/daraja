// Command notesfixture serves the synthetic notes application over HTTPS
// with a freshly generated, throwaway certificate authority. It is a test
// fixture for runtime environments (such as a Nanos guest) that must reach
// the source over HTTPS. It holds only synthetic data.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zati-run/daraja/internal/notesapp"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:9443", "listen address")
	ips := flag.String("ips", "127.0.0.1,10.0.2.2", "comma-separated IP SANs")
	caOut := flag.String("ca-out", "ca.pem", "where to write the CA certificate")
	slow := flag.String("slow-title", "Slow synthetic note", "create title that answers slowly")
	slowFor := flag.Duration("slow-for", 8*time.Second, "delay for the slow title")
	plain := flag.Bool("plain", false, "serve plain HTTP (for a loopback origin) instead of HTTPS")
	flag.Parse()
	if *plain {
		app := notesapp.New("synthetic-user", "synthetic-pass")
		app.SlowTitle, app.SlowFor = *slow, *slowFor
		log.Printf("notes fixture listening on http://%s", *listen)
		srv := &http.Server{Addr: *listen, ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("fixture %s %s", r.Method, r.URL.Path)
			app.ServeHTTP(w, r)
		})}
		log.Fatal(srv.ListenAndServe())
	}

	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "daraja synthetic test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		log.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "notes fixture"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"},
	}
	for _, s := range strings.Split(*ips, ",") {
		if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil {
			leafTmpl.IPAddresses = append(leafTmpl.IPAddresses, ip)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*caOut, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		log.Fatal(err)
	}
	app := notesapp.New("synthetic-user", "synthetic-pass")
	app.SlowTitle, app.SlowFor = *slow, *slowFor
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("fixture %s %s", r.Method, r.URL.Path)
		app.ServeHTTP(w, r)
	})
	srv := &http.Server{
		Addr: *listen, Handler: logged, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}},
	}
	log.Printf("notes fixture listening on https://%s (CA in %s)", *listen, *caOut)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
