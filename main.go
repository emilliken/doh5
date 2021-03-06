// DNS-over-HTTPS SOCKS5 server
// TODO: set a deadline on socks negotiation?

package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DialTimeout = 16 * time.Second
)

const (
	Socks5Version = 0x05

	Socks5CmdConnect   = 0x01
	Socks5MethodNoAuth = 0x00

	Socks5AtypIPv4    = 0x01
	Socks5AtypeDomain = 0x03
	Socks5AtypIPv6    = 0x04
)

var (
	lFlag = flag.String("l", "1080", "port, ip:port, or ./file to listen and serve on")
	sFlag = flag.String("s", "", "`source` IP to bind to for outgoing connections")
	qFlag = flag.Bool("q", false, "enable quiet mode")
	rFlag = flag.String("r", "cloudflare", "DoH `service` [ cloudflare, google, cloudflare-tor, none ]")
)

func socksHandle(c net.Conn, dial *net.Dialer) {
	defer c.Close()
	nc, err := socksNegotiate(c, dial)
	if err != nil {
		log.Printf("peer %v: %v\n", c.RemoteAddr(), err)
		return
	}
	go func() {
		io.Copy(nc, c)
		nc.Close()
	}()
	io.Copy(c, nc)
}

func socksNegotiate(c net.Conn, dial *net.Dialer) (net.Conn, error) {
	buf := make([]byte, 2048)
	_, err := io.ReadFull(c, buf[:2])
	if err != nil {
		return nil, err
	}
	if buf[0] != Socks5Version {
		return nil, errors.New("bad socks version")
	}
	n := buf[1]
	_, err = io.ReadFull(c, buf[:n])
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(buf[:n], Socks5MethodNoAuth) < 0 {
		return nil, errors.New("no supported methods found.")
	}
	_, err = c.Write([]byte{Socks5Version, Socks5MethodNoAuth})
	if err != nil {
		return nil, err
	}
	_, err = io.ReadFull(c, buf[:4])
	if err != nil {
		return nil, err
	}
	if buf[0] != Socks5Version || buf[1] != Socks5CmdConnect {
		return nil, errors.New("socks error")
	}
	var dest string
	switch buf[3] {
	case Socks5AtypIPv4:
		_, err = io.ReadFull(c, buf[:4])
		if err != nil {
			return nil, err
		}
		var ip net.IP = buf[:4]
		dest = ip.String()
	case Socks5AtypeDomain:
		_, err := c.Read(buf[:1])
		if err != nil {
			return nil, err
		}
		n = buf[0]
		_, err = io.ReadFull(c, buf[:n])
		if err != nil {
			return nil, err
		}
		dest = string(buf[:n])
	case Socks5AtypIPv6:
		_, err = io.ReadFull(c, buf[:16])
		if err != nil {
			return nil, err
		}
		var ip net.IP = buf[:16]
		dest = ip.String()
	}
	_, err = io.ReadFull(c, buf[:2])
	if err != nil {
		return nil, err
	}
	port := int(buf[1]) | (int(buf[0]) << 8)
	raddr := net.JoinHostPort(dest, strconv.Itoa(port))

	log.Printf("-> %s . %d\n", dest, port)

	nc, err := dial.Dial("tcp", raddr)
	if err != nil {
		return nil, err
	}
	_, err = c.Write([]byte("\x05\x00\x00\x01\x00\x00\x00\x00\x00\x00"))
	if err != nil {
		nc.Close()
		return nil, err
	}
	return nc, err
}

// parse a [opt:]req argument with a default for the opt
func OptPrefix(arg string, def string) (string, string) {
	args := strings.SplitN(arg, ":", 2)
	if len(args) == 1 {
		return def, args[0]
	}
	return args[0], args[1]
}

func parseAddr(addr string) (string, string, error) {
	if _, err := strconv.ParseUint(addr, 10, 16); err == nil {
		// port
		return "tcp", net.JoinHostPort("127.0.0.1", addr), nil
	} else if strings.Contains(addr, "/") || strings.HasPrefix(addr, "@") {
		// unix file
		return "unix", addr, nil
	} else if _, _, err := net.SplitHostPort(addr); err == nil {
		// ip:port
		return "tcp", addr, nil
	}
	return "", "", errors.New("invalid address given")
}

func main() {
	flag.Parse()

	var ln net.Listener
	var err error

	lNet, lAddr, err := parseAddr(*lFlag)
	if err != nil {
		log.Fatal(err)
	}
	if lNet == "unix" {
		if _, err := os.Stat(lAddr); err == nil {
			fmt.Printf("warning:  file %s exists.  OK to remove? [y] ", lAddr)
			s, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil {
				log.Fatal(err)
			}
			s = strings.TrimSpace(s)
			if s == "" || s[0] == 'Y' || s[0] == 'y' {
				os.Remove(lAddr)
			}
		}
	}
	ln, err = net.Listen(lNet, lAddr)
	if err != nil {
		log.Fatal(err)
	}
	if lNet == "unix" {
		ln.(*net.UnixListener).SetUnlinkOnClose(true)
	}

	bAddr, err := net.ResolveTCPAddr("tcp", *sFlag+":0")
	if err != nil {
		log.Fatal(err)
	}

	res, err := NewResolver(*rFlag)
	if err != nil {
		log.Fatal(err)
	}

	// put this print before entering quiet mode
	log.Printf("SOCKS5 listening on %v with outgoing connections via %v\n", ln.Addr(), bAddr)

	if *qFlag {
		log.Printf("quiet mode enabled\n")
		log.SetOutput(ioutil.Discard)
	}

	dial := &net.Dialer{
		Timeout:   DialTimeout,
		LocalAddr: bAddr,
		DualStack: true,
		Resolver:  res,
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				log.Println(err)
				continue
			}
			log.Fatal(err)
		}
		go socksHandle(c, dial)
	}
}
