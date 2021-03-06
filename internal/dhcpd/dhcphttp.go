package dhcpd

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/AdguardTeam/AdGuardHome/internal/sysutil"
	"github.com/AdguardTeam/AdGuardHome/internal/util"
	"github.com/AdguardTeam/golibs/jsonutil"
	"github.com/AdguardTeam/golibs/log"
)

func httpError(r *http.Request, w http.ResponseWriter, code int, format string, args ...interface{}) {
	text := fmt.Sprintf(format, args...)
	log.Info("DHCP: %s %s: %s", r.Method, r.URL, text)
	http.Error(w, text, code)
}

type v4ServerConfJSON struct {
	GatewayIP     net.IP `json:"gateway_ip"`
	SubnetMask    net.IP `json:"subnet_mask"`
	RangeStart    net.IP `json:"range_start"`
	RangeEnd      net.IP `json:"range_end"`
	LeaseDuration uint32 `json:"lease_duration"`
}

func v4JSONToServerConf(j v4ServerConfJSON) V4ServerConf {
	return V4ServerConf{
		GatewayIP:     j.GatewayIP,
		SubnetMask:    j.SubnetMask,
		RangeStart:    j.RangeStart,
		RangeEnd:      j.RangeEnd,
		LeaseDuration: j.LeaseDuration,
	}
}

type v6ServerConfJSON struct {
	RangeStart    net.IP `json:"range_start"`
	LeaseDuration uint32 `json:"lease_duration"`
}

func v6JSONToServerConf(j v6ServerConfJSON) V6ServerConf {
	return V6ServerConf{
		RangeStart:    j.RangeStart,
		LeaseDuration: j.LeaseDuration,
	}
}

// dhcpStatusResponse is the response for /control/dhcp/status endpoint.
type dhcpStatusResponse struct {
	Enabled      bool         `json:"enabled"`
	IfaceName    string       `json:"interface_name"`
	V4           V4ServerConf `json:"v4"`
	V6           V6ServerConf `json:"v6"`
	Leases       []Lease      `json:"leases"`
	StaticLeases []Lease      `json:"static_leases"`
}

func (s *Server) handleDHCPStatus(w http.ResponseWriter, r *http.Request) {
	status := &dhcpStatusResponse{
		Enabled:   s.conf.Enabled,
		IfaceName: s.conf.InterfaceName,
		V4:        V4ServerConf{},
		V6:        V6ServerConf{},
	}

	s.srv4.WriteDiskConfig4(&status.V4)
	s.srv6.WriteDiskConfig6(&status.V6)

	status.Leases = s.Leases(LeasesDynamic)
	status.StaticLeases = s.Leases(LeasesStatic)

	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(status)
	if err != nil {
		httpError(r, w, http.StatusInternalServerError, "Unable to marshal DHCP status json: %s", err)
		return
	}
}

type dhcpServerConfigJSON struct {
	Enabled       bool             `json:"enabled"`
	InterfaceName string           `json:"interface_name"`
	V4            v4ServerConfJSON `json:"v4"`
	V6            v6ServerConfJSON `json:"v6"`
}

func (s *Server) handleDHCPSetConfig(w http.ResponseWriter, r *http.Request) {
	newconfig := dhcpServerConfigJSON{}
	newconfig.Enabled = s.conf.Enabled
	newconfig.InterfaceName = s.conf.InterfaceName

	js, err := jsonutil.DecodeObject(&newconfig, r.Body)
	if err != nil {
		httpError(r, w, http.StatusBadRequest, "Failed to parse new DHCP config json: %s", err)
		return
	}

	var s4 DHCPServer
	var s6 DHCPServer
	v4Enabled := false
	v6Enabled := false

	if js.Exists("v4") {
		v4conf := v4JSONToServerConf(newconfig.V4)
		v4conf.Enabled = newconfig.Enabled
		if len(v4conf.RangeStart) == 0 {
			v4conf.Enabled = false
		}
		v4Enabled = v4conf.Enabled
		v4conf.InterfaceName = newconfig.InterfaceName

		c4 := V4ServerConf{}
		s.srv4.WriteDiskConfig4(&c4)
		v4conf.notify = c4.notify
		v4conf.ICMPTimeout = c4.ICMPTimeout

		s4, err = v4Create(v4conf)
		if err != nil {
			httpError(r, w, http.StatusBadRequest, "Invalid DHCPv4 configuration: %s", err)
			return
		}
	}

	if js.Exists("v6") {
		v6conf := v6JSONToServerConf(newconfig.V6)
		v6conf.Enabled = newconfig.Enabled
		if len(v6conf.RangeStart) == 0 {
			v6conf.Enabled = false
		}
		v6Enabled = v6conf.Enabled
		v6conf.InterfaceName = newconfig.InterfaceName
		v6conf.notify = s.onNotify
		s6, err = v6Create(v6conf)
		if err != nil {
			httpError(r, w, http.StatusBadRequest, "Invalid DHCPv6 configuration: %s", err)
			return
		}
	}

	if newconfig.Enabled && !v4Enabled && !v6Enabled {
		httpError(r, w, http.StatusBadRequest, "DHCPv4 or DHCPv6 configuration must be complete")
		return
	}

	s.Stop()

	if js.Exists("enabled") {
		s.conf.Enabled = newconfig.Enabled
	}

	if js.Exists("interface_name") {
		s.conf.InterfaceName = newconfig.InterfaceName
	}

	if s4 != nil {
		s.srv4 = s4
	}
	if s6 != nil {
		s.srv6 = s6
	}
	s.conf.ConfigModified()
	s.dbLoad()

	if s.conf.Enabled {
		staticIP, err := sysutil.IfaceHasStaticIP(newconfig.InterfaceName)
		if !staticIP && err == nil {
			err = sysutil.IfaceSetStaticIP(newconfig.InterfaceName)
			if err != nil {
				httpError(r, w, http.StatusInternalServerError, "Failed to configure static IP: %s", err)
				return
			}
		}

		err = s.Start()
		if err != nil {
			httpError(r, w, http.StatusBadRequest, "Failed to start DHCP server: %s", err)
			return
		}
	}
}

type netInterfaceJSON struct {
	Name         string   `json:"name"`
	GatewayIP    net.IP   `json:"gateway_ip"`
	HardwareAddr string   `json:"hardware_address"`
	Addrs4       []net.IP `json:"ipv4_addresses"`
	Addrs6       []net.IP `json:"ipv6_addresses"`
	Flags        string   `json:"flags"`
}

func (s *Server) handleDHCPInterfaces(w http.ResponseWriter, r *http.Request) {
	response := map[string]netInterfaceJSON{}

	ifaces, err := util.GetValidNetInterfaces()
	if err != nil {
		httpError(r, w, http.StatusInternalServerError, "Couldn't get interfaces: %s", err)
		return
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			// it's a loopback, skip it
			continue
		}
		if iface.Flags&net.FlagBroadcast == 0 {
			// this interface doesn't support broadcast, skip it
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			httpError(r, w, http.StatusInternalServerError, "Failed to get addresses for interface %s: %s", iface.Name, err)
			return
		}

		jsonIface := netInterfaceJSON{
			Name:         iface.Name,
			HardwareAddr: iface.HardwareAddr.String(),
		}

		if iface.Flags != 0 {
			jsonIface.Flags = iface.Flags.String()
		}
		// we don't want link-local addresses in json, so skip them
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				// not an IPNet, should not happen
				httpError(r, w, http.StatusInternalServerError, "SHOULD NOT HAPPEN: got iface.Addrs() element %s that is not net.IPNet, it is %T", addr, addr)
				return
			}
			// ignore link-local
			if ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			if ipnet.IP.To4() != nil {
				jsonIface.Addrs4 = append(jsonIface.Addrs4, ipnet.IP)
			} else {
				jsonIface.Addrs6 = append(jsonIface.Addrs6, ipnet.IP)
			}
		}
		if len(jsonIface.Addrs4)+len(jsonIface.Addrs6) != 0 {
			jsonIface.GatewayIP = sysutil.GatewayIP(iface.Name)
			response[iface.Name] = jsonIface
		}
	}

	err = json.NewEncoder(w).Encode(response)
	if err != nil {
		httpError(r, w, http.StatusInternalServerError, "Failed to marshal json with available interfaces: %s", err)
		return
	}
}

// dhcpSearchOtherResult contains information about other DHCP server for
// specific network interface.
type dhcpSearchOtherResult struct {
	Found string `json:"found,omitempty"`
	Error string `json:"error,omitempty"`
}

// dhcpStaticIPStatus contains information about static IP address for DHCP
// server.
type dhcpStaticIPStatus struct {
	Static string `json:"static"`
	IP     string `json:"ip,omitempty"`
	Error  string `json:"error,omitempty"`
}

// dhcpSearchV4Result contains information about DHCPv4 server for specific
// network interface.
type dhcpSearchV4Result struct {
	OtherServer dhcpSearchOtherResult `json:"other_server"`
	StaticIP    dhcpStaticIPStatus    `json:"static_ip"`
}

// dhcpSearchV6Result contains information about DHCPv6 server for specific
// network interface.
type dhcpSearchV6Result struct {
	OtherServer dhcpSearchOtherResult `json:"other_server"`
}

// dhcpSearchResult is a response for /control/dhcp/find_active_dhcp endpoint.
type dhcpSearchResult struct {
	V4 dhcpSearchV4Result `json:"v4"`
	V6 dhcpSearchV6Result `json:"v6"`
}

// Perform the following tasks:
// . Search for another DHCP server running
// . Check if a static IP is configured for the network interface
// Respond with results
func (s *Server) handleDHCPFindActiveServer(w http.ResponseWriter, r *http.Request) {
	// This use of ReadAll is safe, because request's body is now limited.
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		msg := fmt.Sprintf("failed to read request body: %s", err)
		log.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	interfaceName := strings.TrimSpace(string(body))
	if interfaceName == "" {
		msg := "empty interface name specified"
		log.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	result := dhcpSearchResult{
		V4: dhcpSearchV4Result{
			OtherServer: dhcpSearchOtherResult{},
			StaticIP:    dhcpStaticIPStatus{},
		},
		V6: dhcpSearchV6Result{
			OtherServer: dhcpSearchOtherResult{},
		},
	}

	found4, err4 := CheckIfOtherDHCPServersPresentV4(interfaceName)

	isStaticIP, err := sysutil.IfaceHasStaticIP(interfaceName)
	if err != nil {
		result.V4.StaticIP.Static = "error"
		result.V4.StaticIP.Error = err.Error()
	} else if !isStaticIP {
		result.V4.StaticIP.Static = "no"
		result.V4.StaticIP.IP = util.GetSubnet(interfaceName).String()
	}

	if found4 {
		result.V4.OtherServer.Found = "yes"
	} else if err4 != nil {
		result.V4.OtherServer.Found = "error"
		result.V4.OtherServer.Error = err4.Error()
	}

	found6, err6 := CheckIfOtherDHCPServersPresentV6(interfaceName)

	if found6 {
		result.V6.OtherServer.Found = "yes"
	} else if err6 != nil {
		result.V6.OtherServer.Found = "error"
		result.V6.OtherServer.Error = err6.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(result)
	if err != nil {
		httpError(r, w, http.StatusInternalServerError, "Failed to marshal DHCP found json: %s", err)
		return
	}
}

func (s *Server) handleDHCPAddStaticLease(w http.ResponseWriter, r *http.Request) {
	lj := Lease{}
	err := json.NewDecoder(r.Body).Decode(&lj)
	if err != nil {
		httpError(r, w, http.StatusBadRequest, "json.Decode: %s", err)

		return
	}

	if lj.IP == nil {
		httpError(r, w, http.StatusBadRequest, "invalid IP")

		return
	}

	ip4 := lj.IP.To4()

	if ip4 == nil {
		lj.IP = lj.IP.To16()

		err = s.srv6.AddStaticLease(lj)
		if err != nil {
			httpError(r, w, http.StatusBadRequest, "%s", err)
		}

		return
	}

	lj.IP = ip4
	err = s.srv4.AddStaticLease(lj)
	if err != nil {
		httpError(r, w, http.StatusBadRequest, "%s", err)

		return
	}
}

func (s *Server) handleDHCPRemoveStaticLease(w http.ResponseWriter, r *http.Request) {
	lj := Lease{}
	err := json.NewDecoder(r.Body).Decode(&lj)
	if err != nil {
		httpError(r, w, http.StatusBadRequest, "json.Decode: %s", err)

		return
	}

	if lj.IP == nil {
		httpError(r, w, http.StatusBadRequest, "invalid IP")

		return
	}

	ip4 := lj.IP.To4()

	if ip4 == nil {
		lj.IP = lj.IP.To16()

		err = s.srv6.RemoveStaticLease(lj)
		if err != nil {
			httpError(r, w, http.StatusBadRequest, "%s", err)
		}

		return
	}

	lj.IP = ip4
	err = s.srv4.RemoveStaticLease(lj)
	if err != nil {
		httpError(r, w, http.StatusBadRequest, "%s", err)

		return
	}
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.Stop()

	err := os.Remove(s.conf.DBFilePath)
	if err != nil && !os.IsNotExist(err) {
		log.Error("DHCP: os.Remove: %s: %s", s.conf.DBFilePath, err)
	}

	oldconf := s.conf
	s.conf = ServerConfig{}
	s.conf.WorkDir = oldconf.WorkDir
	s.conf.HTTPRegister = oldconf.HTTPRegister
	s.conf.ConfigModified = oldconf.ConfigModified
	s.conf.DBFilePath = oldconf.DBFilePath

	v4conf := V4ServerConf{}
	v4conf.ICMPTimeout = 1000
	v4conf.notify = s.onNotify
	s.srv4, _ = v4Create(v4conf)

	v6conf := V6ServerConf{}
	v6conf.notify = s.onNotify
	s.srv6, _ = v6Create(v6conf)

	s.conf.ConfigModified()
}

func (s *Server) registerHandlers() {
	s.conf.HTTPRegister(http.MethodGet, "/control/dhcp/status", s.handleDHCPStatus)
	s.conf.HTTPRegister(http.MethodGet, "/control/dhcp/interfaces", s.handleDHCPInterfaces)
	s.conf.HTTPRegister(http.MethodPost, "/control/dhcp/set_config", s.handleDHCPSetConfig)
	s.conf.HTTPRegister(http.MethodPost, "/control/dhcp/find_active_dhcp", s.handleDHCPFindActiveServer)
	s.conf.HTTPRegister(http.MethodPost, "/control/dhcp/add_static_lease", s.handleDHCPAddStaticLease)
	s.conf.HTTPRegister(http.MethodPost, "/control/dhcp/remove_static_lease", s.handleDHCPRemoveStaticLease)
	s.conf.HTTPRegister(http.MethodPost, "/control/dhcp/reset", s.handleReset)
}

// jsonError is a generic JSON error response.
//
// TODO(a.garipov): Merge together with the implementations in .../home and
// other packages after refactoring the web handler registering.
type jsonError struct {
	// Message is the error message, an opaque string.
	Message string `json:"message"`
}

// notImplemented returns a handler that replies to any request with an HTTP 501
// Not Implemented status and a JSON error with the provided message msg.
//
// TODO(a.garipov): Either take the logger from the server after we've
// refactored logging or make this not a method of *Server.
func (s *Server) notImplemented(msg string) (f func(http.ResponseWriter, *http.Request)) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)

		err := json.NewEncoder(w).Encode(&jsonError{
			Message: msg,
		})
		if err != nil {
			log.Debug("writing 501 json response: %s", err)
		}
	}
}

func (s *Server) registerNotImplementedHandlers() {
	h := s.notImplemented("dhcp is not supported on windows")

	s.conf.HTTPRegister(http.MethodGet, "/control/dhcp/status", h)
	s.conf.HTTPRegister(http.MethodGet, "/control/dhcp/interfaces", h)
	s.conf.HTTPRegister(http.MethodPost, "/control/dhcp/set_config", h)
	s.conf.HTTPRegister(http.MethodPost, "/control/dhcp/find_active_dhcp", h)
	s.conf.HTTPRegister(http.MethodPost, "/control/dhcp/add_static_lease", h)
	s.conf.HTTPRegister(http.MethodPost, "/control/dhcp/remove_static_lease", h)
	s.conf.HTTPRegister(http.MethodPost, "/control/dhcp/reset", h)
}
