/*
Copyright 2026 bpalko.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package postgres

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bpalko/sprout/internal/provisioner"
)

func TestProvisionerOverRequiredTLS(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	caPath, certPath, keyPath, err := writeTestTLSMaterial(dir)
	if err != nil {
		t.Fatal(err)
	}
	confPath := filepath.Join(dir, "postgresql.conf")
	conf := []byte("listen_addresses = '*'\n" +
		"ssl = on\n" +
		"ssl_ca_file = '/tmp/testcontainers-go/postgres/ca_cert.pem'\n" +
		"ssl_cert_file = '/tmp/testcontainers-go/postgres/server.cert'\n" +
		"ssl_key_file = '/tmp/testcontainers-go/postgres/server.key'\n")
	if err := os.WriteFile(confPath, conf, 0o644); err != nil {
		t.Fatal(err)
	}

	container, err := tcpostgres.Run(ctx, "postgres:16",
		tcpostgres.WithDatabase(pgSuperuser),
		tcpostgres.WithUsername(pgSuperuser),
		tcpostgres.WithPassword(pgSuperuser),
		tcpostgres.WithConfigFile(confPath),
		tcpostgres.WithSSLCert(caPath, certPath, keyPath),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatal(err)
	}

	conn := provisioner.ConnectionConfig{
		Host:          host,
		Port:          int32(port.Num()),
		AdminUser:     pgSuperuser,
		AdminPassword: pgSuperuser,
		AdminDatabase: pgSuperuser,
		SSLMode:       "require",
	}

	spec := provisioner.DatabaseSpec{DatabaseName: "sprout_ssl_tenant", OwnerID: "ssl-owner"}
	if err := New().EnsureDatabase(ctx, conn, spec); err != nil {
		t.Fatalf("EnsureDatabase over sslmode=require: %v", err)
	}
	exists, err := NewInspector(conn).DatabaseExists(ctx, spec.DatabaseName)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected tenant database to exist after TLS provision")
	}
}

func writeTestTLSMaterial(dir string) (caPath, certPath, keyPath string, err error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", "", err
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sprout-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		return "", "", "", err
	}

	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", "", err
	}
	serverTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTpl, caTpl, &serverKey.PublicKey, caKey)
	if err != nil {
		return "", "", "", err
	}

	caPath = filepath.Join(dir, "ca.pem")
	certPath = filepath.Join(dir, "server.crt")
	keyPath = filepath.Join(dir, "server.key")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		return "", "", "", err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), 0o600); err != nil {
		return "", "", "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)}), 0o600); err != nil {
		return "", "", "", err
	}
	return caPath, certPath, keyPath, nil
}
