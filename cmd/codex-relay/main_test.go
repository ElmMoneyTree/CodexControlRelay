package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDirectTLSProcess(t *testing.T) {
	if os.Getenv("CODEX_RELAY_TLS_TEST_HELPER") == "1" {
		os.Args = []string{"codex-relay", "--config", os.Getenv("CODEX_RELAY_TLS_TEST_CONFIG")}
		os.Exit(run())
	}

	directory := t.TempDir()
	certPath, keyPath := createTestCertificate(t, directory)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	configPath := filepath.Join(directory, "relay.yaml")
	configText := fmt.Sprintf(`
server:
  listen: 127.0.0.1:%d
  public_url: https://127.0.0.1:%d
  tls:
    cert_file: %q
    key_file: %q
database:
  path: relay.sqlite
security:
  control_token: 0123456789abcdef0123456789abcdef
logging:
  level: error
  format: text
`, port, port, filepath.ToSlash(certPath), filepath.ToSlash(keyPath))
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestDirectTLSProcess$")
	command.Env = append(os.Environ(), "CODEX_RELAY_TLS_TEST_HELPER=1", "CODEX_RELAY_TLS_TEST_CONFIG="+configPath)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil || !command.ProcessState.Exited() {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})

	client := &http.Client{
		Timeout:   500 * time.Millisecond,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}, // test-only certificate
	}
	origin := fmt.Sprintf("https://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, requestErr := client.Get(origin + "/ready")
		if requestErr == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("readiness status %d", response.StatusCode)
			}
			if value := response.Header.Get("Strict-Transport-Security"); value != "max-age=31536000" {
				t.Fatalf("unexpected HSTS header %q", value)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("TLS Relay did not become ready: %v", requestErr)
		}
		if command.ProcessState != nil && command.ProcessState.Exited() {
			t.Fatalf("TLS Relay exited before becoming ready")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("expected killed helper process")
	}
}

func createTestCertificate(t *testing.T, directory string) (string, string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(directory, "cert.pem")
	keyPath := filepath.Join(directory, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateBytes}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
