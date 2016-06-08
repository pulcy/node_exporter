// Copyright (c) 2016 Pulcy.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collector

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

const (
	vaultNamespace      = "vault"
	vaultUpdateInternal = time.Second * 30
)

type vaultCollector struct {
	address           string
	client            *http.Client
	vaultUnsealedDesc *prometheus.Desc
	statusLock        sync.Mutex
	status            map[string]float64
}

func init() {
	Factories["vault"] = NewVaultCollector
}

// Take a prometheus registry and return a new Collector exposing status of vault servers
func NewVaultCollector() (Collector, error) {
	addr := os.Getenv("VAULT_ADDR")
	caCert := os.Getenv("VAULT_CACERT")
	caPath := os.Getenv("VAULT_CAPATH")
	serverName := ""
	if addr != "" {
		url, err := url.Parse(addr)
		if err != nil {
			return nil, maskAny(err)
		}
		host, _, err := net.SplitHostPort(url.Host)
		if err != nil {
			return nil, maskAny(err)
		}
		serverName = host
	}
	var certPool *x509.CertPool
	if caCert != "" || caPath != "" {
		var err error
		if caCert != "" {
			log.Debugf("Loading CA cert: %s", caCert)
			certPool, err = LoadCACert(caCert)
		} else {
			log.Debugf("Loading CA certs from: %s", caPath)
			certPool, err = LoadCAPath(caPath)
		}
		if err != nil {
			return nil, maskAny(err)
		}
	}

	transport := cleanhttp.DefaultTransport()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{
		ServerName: serverName,
		RootCAs:    certPool,
		//InsecureSkipVerify: true,
	}
	client := &http.Client{
		Transport: transport,
	}
	c := &vaultCollector{
		address: addr,
		client:  client,
		vaultUnsealedDesc: prometheus.NewDesc(
			prometheus.BuildFQName(vaultNamespace, "", "server_unsealed"),
			"Vault unseal status (1=unsealed, 0=sealed)", []string{"address"}, nil,
		),
		status: make(map[string]float64),
	}
	go c.updateStatusLoop()
	return c, nil
}

func (c *vaultCollector) Update(ch chan<- prometheus.Metric) (err error) {
	if c.address == "" {
		return nil
	}

	// This collector updates the vault status in a background routine because
	// there may be (expected) timeouts in the query.

	c.statusLock.Lock()
	defer c.statusLock.Unlock()
	status := c.status

	for ip, value := range status {
		ch <- prometheus.MustNewConstMetric(
			c.vaultUnsealedDesc, prometheus.GaugeValue, value,
			ip)
	}

	return nil
}

func (c *vaultCollector) updateStatusLoop() {
	if c.address == "" {
		return
	}
	for {
		if err := c.updateStatus(); err != nil {
			log.Warnf("vault: failed to updateStatus: %#v", err)
		}
		time.Sleep(vaultUpdateInternal)
	}
}

func (c *vaultCollector) updateStatus() error {
	url, err := url.Parse(c.address)
	if err != nil {
		return maskAny(err)
	}
	host, port, err := net.SplitHostPort(url.Host)
	if err != nil {
		return maskAny(err)
	}
	// Is the host address already an IP address?
	var ips []net.IP
	ip := net.ParseIP(host)
	if ip != nil {
		// Yes, host address is an IP
		ips = []net.IP{ip}
	} else {
		// Get IP's for host address
		ips, err = net.LookupIP(host)
		if err != nil {
			return maskAny(err)
		}
	}

	status := make(map[string]float64)
	mutex := sync.Mutex{}
	wg := sync.WaitGroup{}
	for _, ip := range ips {
		wg.Add(1)
		go func(ip net.IP) {
			defer wg.Done()
			value, err := c.collectServerStatus(ip, port)
			if err != nil {
				log.Debugf("failed to load statistics for %s: %#v", ip, err)
			}
			mutex.Lock()
			defer mutex.Unlock()
			status[ip.String()] = value
		}(ip)
	}
	wg.Wait()

	c.statusLock.Lock()
	defer c.statusLock.Unlock()
	c.status = status

	return nil
}

func (c *vaultCollector) collectServerStatus(address net.IP, port string) (float64, error) {
	host := net.JoinHostPort(address.String(), port)
	url := fmt.Sprintf("https://%s/v1/sys/seal-status", host)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0.0, maskAny(err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0.0, maskAny(err)
	}
	defer resp.Body.Close()
	raw, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0.0, maskAny(err)
	}
	var data SealStatusResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return 0.0, maskAny(err)
	}
	value := 0.0
	if !data.Sealed {
		value = 1.0
	}

	return value, nil
}

type SealStatusResponse struct {
	Sealed   bool
	T        int
	N        int
	Progress int
}

// Loads the certificate from given path and creates a certificate pool from it.
func LoadCACert(path string) (*x509.CertPool, error) {
	certs, err := loadCertFromPEM(path)
	if err != nil {
		return nil, err
	}

	result := x509.NewCertPool()
	for _, cert := range certs {
		result.AddCert(cert)
	}

	return result, nil
}

// Loads the certificates present in the given directory and creates a
// certificate pool from it.
func LoadCAPath(path string) (*x509.CertPool, error) {
	result := x509.NewCertPool()
	fn := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		certs, err := loadCertFromPEM(path)
		if err != nil {
			return err
		}

		for _, cert := range certs {
			result.AddCert(cert)
		}
		return nil
	}

	return result, filepath.Walk(path, fn)
}

// Creates a certificate from the given path
func loadCertFromPEM(path string) ([]*x509.Certificate, error) {
	pemCerts, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	certs := make([]*x509.Certificate, 0, 5)
	for len(pemCerts) > 0 {
		var block *pem.Block
		block, pemCerts = pem.Decode(pemCerts)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}

		certs = append(certs, cert)
	}

	return certs, nil
}
