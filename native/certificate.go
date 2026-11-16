package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func (s *Store) loadCertificate() error {
	// One atomically replaced file prevents mismatched key/certificate pairs
	// after a power interruption. Every installation creates its own identity.
	path := filepath.Join(s.Data, "server-identity.pem")
	identity, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return err
		}
		template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Neighbour Chat"}, NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().AddDate(5, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			return err
		}
		private, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		identity = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})...)
		if err = atomicWrite(path, identity, 0600); err != nil {
			return err
		}
	} else if e != nil {
		return e
	}
	cert, e := tls.X509KeyPair(identity, identity)
	if e != nil {
		return errors.New("the saved TLS identity is damaged; restore data/server-identity.pem from backup")
	}
	leaf, e := x509.ParseCertificate(cert.Certificate[0])
	if e != nil {
		return e
	}
	if time.Now().After(leaf.NotAfter) {
		return errors.New("the host certificate expired; renew its identity before hosting")
	}
	sum := sha256.Sum256(cert.Certificate[0])
	s.Cert = cert
	s.Fingerprint = hex.EncodeToString(sum[:])
	return nil
}
