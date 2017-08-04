// Command sshpf provides a minimalistic ssh server only allowing port
// forwarding to an (optionally) limited set of addresses.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/artyom/autoflags"

	"golang.org/x/crypto/ssh"
)

func main() {
	args := runArgs{
		AuthKeysFile: "authorized_keys",
		HostKeyFile:  "id_rsa",
		Addr:         "localhost:2022",
	}
	autoflags.Parse(&args)
	if err := run(args); err != nil {
		log.Fatal(err)
	}
}

type runArgs struct {
	AuthKeysFile string `flag:"auth,path to authorized_keys file"`
	HostKeyFile  string `flag:"hostKey,path to private host key file"`
	Addr         string `flag:"addr,address to listen"`
	Destinations string `flag:"allowed,file with list of allowed to connect host:port pairs"`
}

func run(args runArgs) error {
	auth, err := authChecker(args.AuthKeysFile)
	if err != nil {
		return err
	}
	hostKey, err := loadHostKey(args.HostKeyFile)
	if err != nil {
		return err
	}
	var destinations []string
	if args.Destinations != "" {
		ss, err := loadDestinations(args.Destinations)
		if err != nil {
			return err
		}
		destinations = ss
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: auth,
		ServerVersion:     "SSH-2.0-generic",
	}
	config.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", args.Addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go handleConn(conn, config, destinations...)
	}
}

func handleConn(nConn net.Conn, config *ssh.ServerConfig, allowedDestinations ...string) error {
	defer nConn.Close()
	_, chans, reqs, err := ssh.NewServerConn(nConn, config)
	if err != nil {
		return err
	}
	go ssh.DiscardRequests(reqs)
	for newChannel := range chans {
		switch newChannel.ChannelType() {
		default:
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		case "session":
			go handleSession(newChannel)
		case "direct-tcpip":
			go handleDial(newChannel, allowedDestinations...)
		}
	}
	return nil
}

func handleDial(newChannel ssh.NewChannel, allowedDestinations ...string) error {
	host, port, err := decodeHostPortPayload(newChannel.ExtraData())
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if len(allowedDestinations) > 0 {
		for _, dest := range allowedDestinations {
			if addr == dest {
				goto dial
			}
		}
		return newChannel.Reject(ssh.Prohibited, "connection to this address is prohibited")
	}
dial:
	rconn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return newChannel.Reject(ssh.ConnectionFailed, "connection failed")
	}
	defer rconn.Close()
	channel, requests, err := newChannel.Accept()
	if err != nil {
		return err
	}
	defer channel.Close()
	go ssh.DiscardRequests(requests)
	go io.Copy(channel, rconn)
	_, err = io.Copy(rconn, channel)
	return err
}

func handleSession(newChannel ssh.NewChannel) error {
	channel, requests, err := newChannel.Accept()
	if err != nil {
		return err
	}
	defer channel.Close()
	go func(reqs <-chan *ssh.Request) {
		for req := range reqs {
			req.Reply(req.Type == "shell", nil)
		}
	}(requests)
	_, err = io.Copy(ioutil.Discard, channel)
	return err
}

func loadHostKey(name string) (ssh.Signer, error) {
	privateBytes, err := ioutil.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(privateBytes)
}

func authChecker(name string) (func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error), error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var pkeys [][]byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		pk, _, _, _, err := ssh.ParseAuthorizedKey(sc.Bytes())
		if err != nil {
			return nil, err
		}
		pkeys = append(pkeys, pk.Marshal())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		keyBytes := key.Marshal()
		for _, k := range pkeys {
			if bytes.Equal(keyBytes, k) {
				return nil, nil
			}
		}
		return nil, errors.New("no keys matched")
	}, nil
}

func decodeHostPortPayload(b []byte) (host string, port int, err error) {
	// https://tools.ietf.org/html/rfc4254#section-7.2
	if b == nil || len(b) < 4 {
		err = errors.New("invalid payload size")
		return
	}
	slen := int(b[3])
	if len(b) < 4+slen+2+2 {
		err = errors.New("invalid payload size")
		return
	}
	host = string(b[4 : 4+slen])
	port = int(uint32(b[4+slen+2])<<8 + uint32(b[4+slen+2+1]))
	return host, port, nil
}

func loadDestinations(name string) ([]string, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var out []string
	for scanner.Scan() {
		if b := scanner.Bytes(); len(b) == 0 || b[0] == '#' {
			continue
		}
		out = append(out, scanner.Text())
	}
	return out, scanner.Err()
}