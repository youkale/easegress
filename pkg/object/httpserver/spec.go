package httpserver

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"regexp"

	"github.com/megaease/easegateway/pkg/scheduler"
	"github.com/megaease/easegateway/pkg/util/ipfilter"
)

type (
	// Spec describes the HTTPServer.
	Spec struct {
		V string `yaml:"-" v:"parent"`

		scheduler.ObjectMeta `yaml:",inline"`
		Port                 uint16 `yaml:"port" v:"gte=1"`
		KeepAlive            bool   `yaml:"keepAlive"`
		KeepAliveTimeout     string `yaml:"keepAliveTimeout" v:"omitempty,duration,dmin=1s"`
		MaxConnections       uint32 `yaml:"maxConnections" v:"gte=1"`
		HTTPS                bool   `yaml:"https"`
		CertBase64           string `yaml:"certBase64" v:"omitempty,base64"`
		KeyBase64            string `yaml:"keyBase64" v:"omitempty,base64"`
		CacheSize            uint32 `yaml:"cacheSize" v:"omitempty"`

		IPFilter *ipfilter.Spec `yaml:"ipFilter" v:"omitempty"`
		Rules    []Rule         `yaml:"rules" v:"dive"`
	}

	// Rule is first level entry of router.
	Rule struct {
		IPFilter   *ipfilter.Spec `yaml:"ipFilter" v:"omitempty"`
		Host       string         `yaml:"host"`
		HostRegexp string         `yaml:"hostRegexp" v:"omitempty,regexp"`
		Paths      []Path         `yaml:"paths" v:"dive"`

		hostRE *regexp.Regexp
	}

	// Path is second level entry of router.
	Path struct {
		IPFilter   *ipfilter.Spec `yaml:"ipFilter" v:"omitempty"`
		Path       string         `yaml:"path,omitempty" v:"omitempty,prefix=/"`
		PathPrefix string         `yaml:"pathPrefix,omitempty" v:"omitempty,prefix=/"`
		PathRegexp string         `yaml:"pathRegexp,omitempty" v:"omitempty,regexp"`
		Methods    []string       `yaml:"methods,omitempty" v:"unique,dive,httpmethod"`
		Backend    string         `yaml:"backend" v:"required"`

		pathRE *regexp.Regexp
	}
)

// Validate validates HTTPServerSpec.
func (spec *Spec) Validate() error {
	if spec.HTTPS {
		if spec.CertBase64 == "" {
			return fmt.Errorf("cert is empty when https enabled")
		}
		if spec.KeyBase64 == "" {
			return fmt.Errorf("key is empty when https enabled")
		}
		_, err := spec.tlsConfig()
		if err != nil {
			return err
		}
	}

	return nil
}

func (spec *Spec) tlsConfig() (*tls.Config, error) {
	certPem, _ := base64.StdEncoding.DecodeString(spec.CertBase64)
	keyPem, _ := base64.StdEncoding.DecodeString(spec.KeyBase64)

	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		return nil, fmt.Errorf("generate x509 key pair failed: %v", err)
	}

	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}
