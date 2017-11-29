// Package pfdns implements a plugin that returns details about the resolving
// querying it.
package pfdns

import (
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/inverse-inc/packetfence/go/coredns/plugin"
	"github.com/inverse-inc/packetfence/go/coredns/request"
	//Import mysql driver
	_ "github.com/go-sql-driver/mysql"
	"github.com/inverse-inc/packetfence/go/pfconfigdriver"
	"github.com/miekg/dns"
	"golang.org/x/net/context"
)

type pfdns struct {
	RedirectIP        net.IP
	Enforce           bool // whether DNS enforcement is enabled
	Db                *sql.DB
	IP4log            *sql.Stmt // prepared statement for ip4log queries
	IP6log            *sql.Stmt // prepared statement for ip6log queries
	Nodedb            *sql.Stmt // prepared statement for node table queries
	Violation         *sql.Stmt // prepared statement for violation
	Bh                bool      //  whether blackholing is enabled or not
	BhIP              net.IP
	BhCname           string
	Next              plugin.Handler
	Webservices       pfconfigdriver.PfConfWebservices
	FqdnPort          map[*regexp.Regexp][]string
	FqdnIsolationPort map[*regexp.Regexp][]string
	FqdnDomainPort    map[*regexp.Regexp][]string
	Network           map[*net.IPNet]net.IP
}

// Ports array
type Ports struct {
	Port map[int]string
}
type dbConf struct {
	DBHost     string `json:"host"`
	DBPort     string `json:"port"`
	DBUser     string `json:"user"`
	DBPassword string `json:"pass"`
	DB         string `json:"db"`
}

// ServeDNS implements the middleware.Handler interface.
func (pf pfdns) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	a := new(dns.Msg)
	a.SetReply(r)
	a.Compress = true
	a.Authoritative = true
	var rr dns.RR

	if pf.Enforce {
		var ipVersion int
		srcIP := state.IP()
		bIP := net.ParseIP(srcIP)
		if bIP.To4() == nil {
			ipVersion = 6
		} else {
			ipVersion = 4
		}

		var mac string
		if ipVersion == 4 {
			err := pf.IP4log.QueryRow(srcIP).Scan(&mac)
			if err != nil {
				fmt.Printf("ERROR pfdns database query returned %s\n", err)
			}
		} else {
			err := pf.IP6log.QueryRow(srcIP).Scan(&mac)
			if err != nil {
				fmt.Printf("ERROR pfdns database query returned %s\n", err)
			}
		}

		var violation bool
		err := pf.Violation.QueryRow(mac).Scan(&violation)
		if err != nil {
			fmt.Printf("ERROR pfdns database query returned %s\n", err)
		}

		if violation {
			// Passthrough bypass
			for k, v := range pf.FqdnIsolationPort {
				if k.MatchString(state.QName()) {
					// if pf.checkPassthrough(ctx, state.QName()) {
					answer, _ := pf.LocalResolver(state)
					for _, ans := range answer.Answer {
						switch ansb := ans.(type) {
						case *dns.A:
							for _, valeur := range v {
								var body io.Reader
								err := pf.post("https://127.0.0.1:22223/ipsetpassthroughisolation/"+ansb.A.String()+"/"+valeur+"/1", body)
								if err != nil {
									fmt.Println("Not able to contact localhost")
								}
							}
						}
					}
					fmt.Println(mac + " isolation passthrough")
					return pf.Next.ServeDNS(ctx, w, r)
				}
			}
		}

		// Passthrough bypass
		for k, v := range pf.FqdnPort {
			if k.MatchString(state.QName()) {
				// if pf.checkPassthrough(ctx, state.QName()) {
				answer, _ := pf.LocalResolver(state)
				for _, ans := range answer.Answer {
					switch ansb := ans.(type) {
					case *dns.A:
						for _, valeur := range v {
							var body io.Reader
							err := pf.post("https://127.0.0.1:22223/ipsetpassthrough/"+ansb.A.String()+"/"+valeur+"/1", body)
							if err != nil {
								fmt.Println("Not able to contact localhost")
							}
						}
					}
				}
				fmt.Println(mac + " passthrough")
				return pf.Next.ServeDNS(ctx, w, r)
			}
		}

		// Domain bypass
		for k, v := range pf.FqdnDomainPort {
			if k.MatchString(state.QName()) {
				answer, _ := pf.LocalResolver(state)
				for _, ans := range answer.Answer {
					switch ansb := ans.(type) {
					case *dns.A:
						for _, valeur := range v {
							var body io.Reader
							err := pf.post("https://127.0.0.1:22223/ipsetpassthrough/"+ansb.A.String()+"/"+valeur+"/1", body)
							if err != nil {
								fmt.Println("Not able to contact localhost")
							}
						}
					}
				}
				fmt.Println(mac + " Domain bypass")
				return pf.Next.ServeDNS(ctx, w, r)
			}
		}

		var status = "unreg"
		err = pf.Nodedb.QueryRow(mac).Scan(&status)
		if err != nil {
			fmt.Printf("ERROR pfdns database query returned %s\n", err)
		}

		// Defer to the proxy middleware if the device is registered
		if status == "reg" {
			fmt.Println(mac + " serve dns")
			return pf.Next.ServeDNS(ctx, w, r)
		}
	}

	// Not allowed through enforcement, or not enforcing.
	// We only handle A and AAAA requests, all others are subject to blackholing.
	// if state.QType() != dns.TypeA && state.QType() != dns.TypeAAAA {
	// 	// blackhole
	// 	if pf.Bh {
	// 		rr = new(dns.CNAME)
	// 		rr.(*dns.CNAME).Hdr = dns.RR_Header{
	// 			Name:   state.QName(),
	// 			Rrtype: dns.TypeCNAME,
	// 			Class:  state.QClass(),
	// 		}
	// 		rr.(*dns.CNAME).Target = pf.BhCname
	//
	// 		var ar dns.RR
	// 		switch state.Family() {
	// 		case 1: // ipv4
	// 			ar = new(dns.A)
	// 			ar.(*dns.A).Hdr = dns.RR_Header{
	// 				Name:   pf.BhCname,
	// 				Rrtype: dns.TypeA,
	// 				Class:  state.QClass(),
	// 			}
	// 			ar.(*dns.A).A = pf.BhIP.To4()
	// 		case 2: // ipv6
	// 			ar = new(dns.AAAA)
	// 			ar.(*dns.AAAA).Hdr = dns.RR_Header{
	// 				Name:   pf.BhCname,
	// 				Rrtype: dns.TypeAAAA,
	// 				Class:  state.QClass(),
	// 			}
	// 			ar.(*dns.AAAA).AAAA = pf.BhIP.To16()
	// 		}
	//
	// 		a.Answer = []dns.RR{rr}
	// 		a.Extra = []dns.RR{ar}
	//
	// 		state.SizeAndDo(a)
	// 		w.WriteMsg(a)
	//
	// 		return 0, nil
	// 	}
	//
	// 	return pf.Next.ServeDNS(ctx, w, r)
	//
	// }

	// Not blackholed, not allowed through due to DNS enforcement.
	// Let's redirect to RedirectIP

	switch state.Family() {
	case 1:
		rr = new(dns.A)
		rr.(*dns.A).Hdr = dns.RR_Header{Name: state.QName(), Rrtype: dns.TypeA, Class: state.QClass()}
		for k, v := range pf.Network {
			if k.Contains(net.ParseIP(state.IP())) {
				rr.(*dns.A).A = v
			} else {
				rr.(*dns.A).A = pf.RedirectIP.To4()
			}
		}
	case 2:
		rr = new(dns.AAAA)
		rr.(*dns.AAAA).Hdr = dns.RR_Header{Name: state.QName(), Rrtype: dns.TypeAAAA, Class: state.QClass()}
		rr.(*dns.AAAA).AAAA = pf.RedirectIP.To16()
	}

	a.Answer = []dns.RR{rr}
	fmt.Println("return portal")
	state.SizeAndDo(a)
	w.WriteMsg(a)

	return 0, nil
}

// Name implements the Handler interface.
func (pf pfdns) Name() string { return "pfdns" }

func readConfig(ctx context.Context) pfconfigdriver.PfconfigDatabase {
	var sections pfconfigdriver.PfconfigDatabase
	sections.PfconfigNS = "config::Pf"
	sections.PfconfigMethod = "hash_element"
	sections.PfconfigHashNS = "database"

	pfconfigdriver.FetchDecodeSocket(ctx, &sections)
	return sections
}

func connectDB(configDatabase pfconfigdriver.PfconfigDatabase) (*sql.DB, error) {
	db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s:%s)/%s",
		configDatabase.DBUser,
		configDatabase.DBPassword,
		configDatabase.DBHost,
		configDatabase.DBPort,
		configDatabase.DBName,
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "pfdns: database connection error: %s", err)
		return nil, err
	}
	return db, nil
}

// DetectVIP
func (pf *pfdns) detectVIP() error {
	var ctx = context.Background()
	var NetIndex net.IPNet
	pf.Network = make(map[*net.IPNet]net.IP)

	var interfaces pfconfigdriver.ListenInts
	pfconfigdriver.FetchDecodeSocket(ctx, &interfaces)

	var keyConfNet pfconfigdriver.PfconfigKeys
	keyConfNet.PfconfigNS = "config::Network"
	pfconfigdriver.FetchDecodeSocket(ctx, &keyConfNet)

	var keyConfCluster pfconfigdriver.NetInterface
	keyConfCluster.PfconfigNS = "config::Pf(CLUSTER)"

	for _, v := range interfaces.Element {

		keyConfCluster.PfconfigHashNS = "interface " + v
		pfconfigdriver.FetchDecodeSocket(ctx, &keyConfCluster)
		// Nothing in keyConfCluster.Ip so we are not in cluster mode
		var VIP net.IP

		eth, _ := net.InterfaceByName(v)
		adresses, _ := eth.Addrs()
		for _, adresse := range adresses {
			var NetIP *net.IPNet
			var IP net.IP
			IP, NetIP, _ = net.ParseCIDR(adresse.String())
			a, b := NetIP.Mask.Size()
			if a == b {
				continue
			}
			if keyConfCluster.Ip != "" {
				VIP = net.ParseIP(keyConfCluster.Ip)
			} else {
				VIP = IP
			}
			for _, key := range keyConfNet.Keys {
				var ConfNet pfconfigdriver.NetworkConf
				ConfNet.PfconfigHashNS = key
				pfconfigdriver.FetchDecodeSocket(ctx, &ConfNet)
				if (NetIP.Contains(net.ParseIP(ConfNet.DhcpStart)) && NetIP.Contains(net.ParseIP(ConfNet.DhcpEnd))) || NetIP.Contains(net.ParseIP(ConfNet.NextHop)) {
					NetIndex.Mask = net.IPMask(net.ParseIP(ConfNet.Netmask))
					NetIndex.IP = net.ParseIP(key)
					Index := NetIndex
					pf.Network[&Index] = VIP
				}
				if ConfNet.RegNetwork != "" {
					IP2, NetIP2, _ := net.ParseCIDR(ConfNet.RegNetwork)
					if NetIP.Contains(IP2) {
						pf.Network[NetIP2] = VIP
					}
				}
			}
		}
	}
	return nil
}

func (pf *pfdns) DomainPassthroughInit() error {
	var ctx = context.Background()
	var keyConfDNS pfconfigdriver.PfconfigKeys
	keyConfDNS.PfconfigNS = "resource::domain_dns_servers"

	pf.FqdnDomainPort = make(map[*regexp.Regexp][]string)
	pfconfigdriver.FetchDecodeSocket(ctx, &keyConfDNS)

	for _, v := range keyConfDNS.Keys {
		// .*_msdcs.
		rgx, _ := regexp.Compile(".*(_msdcs|_sites)." + v)
		pf.FqdnDomainPort[rgx] = []string{"udp:88", "udp:123", "udp:135", "135", "udp:137", "udp:138", "139", "udp:389", "udp:445", "445", "udp:464", "464", "tcp:1025", "49155", "49156", "49172"}
	}

	return nil

}

// WebservicesInit read pfconfig webservices configuration
func (pf *pfdns) WebservicesInit() error {
	var ctx = context.Background()
	var webservices pfconfigdriver.PfConfWebservices
	webservices.PfconfigNS = "config::Pf"
	webservices.PfconfigMethod = "hash_element"
	webservices.PfconfigHashNS = "webservices"

	pfconfigdriver.FetchDecodeSocket(ctx, &webservices)
	pf.Webservices = webservices
	return nil

}

func (pf *pfdns) PassthrouthsInit() error {
	var ctx = context.Background()

	var wildcard pfconfigdriver.PassthroughsConf
	wildcard.PfconfigArray = "wildcard"
	pfconfigdriver.FetchDecodeSocket(ctx, &wildcard)

	pf.FqdnPort = make(map[*regexp.Regexp][]string)

	for k, v := range wildcard.Wildcard {
		rgx, _ := regexp.Compile(".*" + k)
		pf.FqdnPort[rgx] = v
	}

	for k, v := range wildcard.Normal {
		rgx, _ := regexp.Compile(k)
		pf.FqdnPort[rgx] = v
	}
	return nil
}

func (pf *pfdns) PassthrouthsIsolationInit() error {
	var ctx = context.Background()

	var wildcard pfconfigdriver.PassthroughsIsolationConf
	wildcard.PfconfigArray = "wildcard"
	pfconfigdriver.FetchDecodeSocket(ctx, &wildcard)

	pf.FqdnIsolationPort = make(map[*regexp.Regexp][]string)

	for k, v := range wildcard.Wildcard {
		rgx, _ := regexp.Compile(".*" + k)
		pf.FqdnIsolationPort[rgx] = v
	}

	for k, v := range wildcard.Normal {
		rgx, _ := regexp.Compile(k)
		pf.FqdnIsolationPort[rgx] = v
	}
	return nil
}

func (pf *pfdns) DbInit() error {
	var ctx = context.Background()

	var err error
	configDatabase := readConfig(ctx)
	pf.Db, err = connectDB(configDatabase)

	if err != nil {
		// logging the error is handled in connectDB
		return err
	}

	pf.IP4log, err = pf.Db.Prepare("Select MAC from ip4log where IP = ? ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pfdns: database ip4log prepared statement error: %s", err)
		return err
	}

	pf.IP6log, err = pf.Db.Prepare("Select MAC from ip6log where IP = ? ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pfdns: database ip6log prepared statement error: %s", err)
		return err
	}

	pf.Nodedb, err = pf.Db.Prepare("Select STATUS from node where MAC = ? ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pfdns: database nodedb prepared statement error: %s", err)
		return err
	}

	pf.Violation, err = pf.Db.Prepare("Select count(*) from violation, action where violation.vid=action.vid and action.action='reevaluate_access' and mac=? and status='open'")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pfdns: database violation prepared statement error: %s", err)
		return err
	}

	if err != nil {
		fmt.Printf("Error while connecting to database: %s", err)
		return err
	}

	return nil
}

func (pf pfdns) post(url string, body io.Reader) error {
	req, err := http.NewRequest("POST", url, body)
	req.SetBasicAuth(pf.Webservices.User, pf.Webservices.Pass)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	req.Header.Set("Content-Type", "application/json")
	cli := &http.Client{Transport: tr}
	_, err = cli.Do(req)
	return err
}

func (pf pfdns) LocalResolver(request request.Request) (*dns.Msg, error) {
	const (
		// DefaultTimeout is default timeout many operation in this program will
		// use.
		DefaultTimeout time.Duration = 5 * time.Second
	)
	var (
		localc *dns.Client
	)
	localm := request.Req

	localc = &dns.Client{
		ReadTimeout: DefaultTimeout,
	}

	r, _, err := localc.Exchange(localm, "127.0.0.1:54")
	if err != nil {
		return nil, err
	}
	if r == nil || r.Rcode == dns.RcodeNameError || r.Rcode == dns.RcodeSuccess {
		return r, err
	}
	return nil, errors.New("No name server to answer the question")
}
