package main

import (
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/miekg/dns"
	"github.com/pmylund/go-cache"
	"log"
	"net"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var (
	dnss    = flag.String("dns", "61.153.177.200:53:udp,61.153.177.202:53:udp,202.181.224.2:53:udp,8.8.8.8:53:udp,8.8.4.4:53:udp", "dns address, use `,` as sep")
	local   = flag.String("local", ":53", "local listen address")
	debug   = flag.Int("debug", 1, "debug level 0 1 2")
	encache = flag.Bool("cache", true, "enable go-cache")
	expire  = flag.Int64("expire", 3600, "default cache expire seconds, -1 means use doamin ttl time")
	file    = flag.String("file", filepath.Join(path.Dir(os.Args[0]), "cache.dat"), "cached file")
	ipv6    = flag.Bool("6", false, "skip ipv6 record query AAAA")
	timeout = flag.Int("timeout", 1000, "read/write timeout")

	// 域名优先使用的dns服务器
	priorDomainDns map[string]string

	clientTCP *dns.Client
	clientUDP *dns.Client

	DEBUG   int
	ENCACHE bool

	DNS [][]string

	conn *cache.Cache

	saveSig = make(chan os.Signal)
)

func toMd5(data string) string {
	m := md5.New()
	m.Write([]byte(data))
	return hex.EncodeToString(m.Sum(nil))
}

func intervalSaveCache() {
	save := func() {
		err := conn.SaveFile(*file)
		if err == nil {
			log.Printf("cache saved: %s\n", *file)
		} else {
			log.Printf("cache save failed: %s, %s\n", *file, err)
		}
	}
	go func() {
		for {
			select {
			case sig := <-saveSig:
				save()
				switch sig {
				case syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT:
					os.Exit(0)
				case syscall.SIGHUP:
					log.Println("recv SIGHUP clear cache")
					conn.Flush()
				}
			case <-time.After(time.Second * 60):
				save()
			}
		}
	}()
}

func init() {
	flag.Parse()

	ENCACHE = *encache
	DEBUG = *debug

	runtime.GOMAXPROCS(runtime.NumCPU()*2 - 1)
	// 设置两个dns client
	clientTCP = new(dns.Client)
	clientTCP.Net = "tcp"
	clientTCP.ReadTimeout = time.Duration(*timeout) * time.Millisecond
	clientTCP.WriteTimeout = time.Duration(*timeout) * time.Millisecond

	clientUDP = new(dns.Client)
	clientUDP.Net = "udp"
	clientUDP.ReadTimeout = time.Duration(*timeout) * time.Millisecond
	clientUDP.WriteTimeout = time.Duration(*timeout) * time.Millisecond

	if ENCACHE {
		conn = cache.New(time.Second*time.Duration(*expire), time.Second*60)
		conn.LoadFile(*file)
		intervalSaveCache()
	}

	for _, s := range strings.Split(*dnss, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		dns := s
		proto := "udp"
		parts := strings.Split(s, ":")
		if len(parts) > 2 {
			dns = strings.Join(parts[:2], ":")
			if parts[2] == "tcp" {
				proto = "tcp"
			}
		}
		// 验证协议格式的
		_, err := net.ResolveTCPAddr("tcp", dns)
		if err != nil {
			log.Fatalf("wrong dns address %s\n", dns)
		}
		DNS = append(DNS, []string{dns, proto})
	}

	if len(DNS) == 0 {
		log.Fatalln("dns address must be not empty")
	}

	// 设置域名优先解析的DNS列表
	priorDomainDns = make(map[string]string)
	// 将苹果的域名全部使用香港的dns来解析
	priorDomainDns["apple.com"] = "202.181.224.2:53:udp,202.45.84.58:53:udp"
	priorDomainDns["google.com"] = "202.181.224.2:53:udp,202.45.84.58:53:udp"

	signal.Notify(saveSig, syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT)
}

func main() {
	dns.HandleFunc(".", proxyServe)

	failure := make(chan error, 1)

	go func(failure chan error) {
		failure <- dns.ListenAndServe(*local, "tcp", nil)
	}(failure)

	go func(failure chan error) {
		failure <- dns.ListenAndServe(*local, "udp", nil)
	}(failure)

	log.Printf("ready for accept connection on tcp/udp %s ...\n", *local)

	fmt.Println(<-failure)
}

func proxyServe(w dns.ResponseWriter, req *dns.Msg) {
	var (
		key       string
		m         *dns.Msg
		err       error
		tried     bool
		data      []byte
		id        uint16
		query     []string
		questions []dns.Question
		used      string
	)

	defer func() {
		if err := recover(); err != nil {
			fmt.Println(err)
		}
	}()

	if req.MsgHdr.Response == true { // supposed responses sent to us are bogus
		return
	}

	query = make([]string, len(req.Question))

	for i, q := range req.Question {
		if q.Qtype != dns.TypeAAAA || *ipv6 {
			questions = append(questions, q)
		}
		query[i] = fmt.Sprintf("(%s %s %s)", q.Name, dns.ClassToString[q.Qclass], dns.TypeToString[q.Qtype])
	}

	if len(questions) == 0 {
		return
	}
	// 替换掉question
	req.Question = questions

	id = req.Id

	req.Id = 0
	key = toMd5(req.String())
	req.Id = id
	// 查询缓存
	if ENCACHE {
		if reply, ok := conn.Get(key); ok {
			data, _ = reply.([]byte)
		}
		if data != nil && len(data) > 0 {
			m = &dns.Msg{}
			m.Unpack(data)
			m.Id = id
			err = w.WriteMsg(m)

			if DEBUG > 0 {
				log.Printf("id: %5d cache: HIT %v\n", id, query)
			}

			goto end
		} else {
			if DEBUG > 0 {
				log.Printf("id: %5d cache: MISS %v\n", id, query)
			}
		}
	}
	// 优先处理的域名
	for _, q := range req.Question {
		for pattern, priorDnsString := range priorDomainDns {
			// 判断是否有优先级的服务器设置
			if match, _ := regexp.MatchString(pattern, q.Name); match {
				// 分割出各个dns配置
				for _, priorDns := range strings.Split(priorDnsString, ",") {
					// 分割出dns的ip:por跟proto
					parts := strings.Split(priorDns, ":")
					proto := parts[2]
					dns := strings.Join(parts[:2], ":")
					client := clientUDP
					if proto == "tcp" {
						client = clientTCP
					}
					m, _, err = client.Exchange(req, dns)
					if err == nil && len(m.Answer) > 0 {
						used = dns
						tried = true
						log.Printf("优先命中 id: %5d resolve: %v %s %s\n", id, query, dns, proto)
						// 只要命中就跳出
						goto priorEnd
					} else {
						log.Println("优先出错：", err)
					}
				}
			}
		}
	}
priorEnd:
	if used == "" {
		for i, parts := range DNS {
			dns := parts[0]
			proto := parts[1]
			tried = i > 0
			if DEBUG > 0 {
				if tried {
					log.Printf("id: %5d try: %v %s %s\n", id, query, dns, proto)
				} else {
					log.Printf("id: %5d resolve: %v %s %s\n", id, query, dns, proto)
				}
			}
			client := clientUDP
			if proto == "tcp" {
				client = clientTCP
			}
			// 请求外部的dns服务器
			m, _, err = client.Exchange(req, dns)
			if err == nil && len(m.Answer) > 0 {
				used = dns
				break
			}
		}
	}

	if err == nil {
		if DEBUG > 0 {
			if tried {
				if len(m.Answer) == 0 {
					log.Printf("id: %5d failed: %v\n", id, query)
				} else {
					log.Printf("id: %5d bingo: %v %s\n", id, query, used)
				}
			}
		}
		data, err = m.Pack()
		if err == nil {
			_, err = w.Write(data)

			if err == nil {
				if ENCACHE {
					m.Id = 0
					data, _ = m.Pack()
					ttl := 0
					if len(m.Answer) > 0 {
						ttl = int(m.Answer[0].Header().Ttl)
						if ttl < 0 {
							ttl = 0
						}
					}
					conn.Set(key, data, time.Second*time.Duration(ttl))
					m.Id = id
					if DEBUG > 0 {
						log.Printf("id: %5d cache: CACHED %v TTL %v\n", id, query, ttl)
					}
				}
			}
		}
	}

end:
	if DEBUG > 1 {
		fmt.Println(req)
		if m != nil {
			fmt.Println(m)
		}
	}
	if err != nil {
		log.Printf("id: %5d error: %v %s\n", id, query, err)
	}

	if DEBUG > 1 {
		fmt.Println("====================================================")
	}
}
