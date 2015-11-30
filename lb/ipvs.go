package lb

import (
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/emilevauge/traefik/types"
	"github.com/kobolog/gorb/core"
	"github.com/kobolog/gorb/util"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

// IpvsBackend is the IPVS load balancer backend
type IpvsBackend struct {
	ipvsContext *core.Context
	url         url.URL
	fwd         http.Handler
	name        string
}

// NewIpvsBackend creates a new backend, load-balancing on multiple servers
func NewIpvsBackend(backendName string, backend *types.Backend, fwd http.Handler, ipvsContext *core.Context) (*IpvsBackend, error) {
	if len(backend.Servers) == 0 {
		return nil, errors.New("Empty server list in backend")
	}

	// get first backend url to get scheme
	// TODO I don't like this, better way?
	var firstServer url.URL
	for _, server := range backend.Servers {
		url, err := url.Parse(server.URL)
		if err != nil {
			return nil, err
		}
		firstServer = *url
		break
	}

	port, err := getFreePort()
	if err != nil {
		return nil, err
	}
	hostIPs, err := util.InterfaceIPs("docker0")
	if err != nil {
		return nil, err
	}
	if len(hostIPs) == 0 {
		return nil, errors.New("No IPs found on interface " + "docker0")
	}
	hostURL := url.URL{
		Scheme: firstServer.Scheme,
		Host:   hostIPs[0].String() + ":" + fmt.Sprint(port),
	}

	name := backendName + "." + hostURL.String()

	serviceOptions := core.ServiceOptions{
		Host:       hostIPs[0].String(),
		Port:       port,
		Protocol:   "tcp",
		Method:     backend.LoadBalancer.Method,
		Persistent: false,
	}
	err = ipvsContext.CreateService(name, &serviceOptions)
	if err != nil {
		return nil, err
	}
	log.Infof("Creating IPVS backend %s", name)

	for serverName, server := range backend.Servers {
		url, err := url.Parse(server.URL)
		if err != nil {
			return nil, err
		}
		host, port, _ := net.SplitHostPort(url.Host)
		uport, _ := strconv.ParseUint(port, 10, 16)
		backendOptions := core.BackendOptions{
			Host:   host,
			Port:   uint16(uport),
			Weight: int32(server.Weight),
			Method: "nat",
		}
		ipvsContext.CreateBackend(name, name+"."+serverName, &backendOptions)
		log.Infof("Creating IPVS server %s %s", name+"."+serverName, url.String())
	}
	ipvsBackend := IpvsBackend{
		ipvsContext: ipvsContext,
		url:         hostURL,
		fwd:         fwd,
		name:        name,
	}

	backend.SetOnDestroy(func() error {
		return ipvsBackend.Close()
	})
	return &ipvsBackend, nil
}

func (ipvs *IpvsBackend) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// make shallow copy of request before changing anything to avoid side effects
	newReq := *req
	newReq.URL.Host = ipvs.url.Host
	newReq.URL.Scheme = ipvs.url.Scheme
	ipvs.fwd.ServeHTTP(w, &newReq)
}

// Close removes IPVS backend service and servers
func (ipvs *IpvsBackend) Close() error {
	service, err := ipvs.ipvsContext.GetService(ipvs.name)
	if err != nil {
		return err
	}
	for _, backend := range service.Backends {
		ipvs.ipvsContext.RemoveBackend(ipvs.name, backend)
		log.Infof("IPVS backend removed %s:%s", ipvs.name, backend)
	}
	ipvs.ipvsContext.RemoveService(ipvs.name)
	log.Infof("IPVS service removed %s", ipvs.name)
	return nil
}

func getFreePort() (uint16, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}

	defer l.Close()

	portStr := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	v, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0, err
	}

	return uint16(v), nil
}
