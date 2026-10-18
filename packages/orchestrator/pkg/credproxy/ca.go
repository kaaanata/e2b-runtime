package credproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type authority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	bundle      string
	mu          sync.Mutex
	leaves      map[string]*tls.Certificate
}
type caDocument struct {
	Certificate []byte `json:"certificate"`
	Key         []byte `json:"key"`
}

func loadAuthority(root string) (*authority, error) {
	lock, err := os.OpenFile(filepath.Join(root, "ca.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	path := filepath.Join(root, "ca.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			return nil, e
		}
		serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if e != nil {
			return nil, e
		}
		tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "sbx credential proxy node CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
		cert, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if e != nil {
			return nil, e
		}
		priv, e := x509.MarshalECPrivateKey(key)
		if e != nil {
			return nil, e
		}
		raw, e = json.Marshal(caDocument{Certificate: cert, Key: priv})
		if e != nil {
			return nil, e
		}
		if e = atomicPrivate(path, raw); e != nil {
			return nil, e
		}
	} else if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("CA must be a regular mode 0600 file")
	}
	var doc caDocument
	if json.Unmarshal(raw, &doc) != nil {
		return nil, errors.New("invalid CA document")
	}
	cert, err := x509.ParseCertificate(doc.Certificate)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(doc.Key)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA || time.Now().After(cert.NotAfter) || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("invalid or expired proxy CA")
	}

	return &authority{certificate: cert, key: key, bundle: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: doc.Certificate})), leaves: make(map[string]*tls.Certificate)}, nil
}

func (a *authority) leaf(host string) (*tls.Certificate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.leaves[host]; ok && time.Until(c.Leaf.NotAfter) > time.Hour {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: minTime(time.Now().Add(24*time.Hour), a.certificate.NotAfter), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.certificate, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	a.leaves[host] = c

	return c, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}
