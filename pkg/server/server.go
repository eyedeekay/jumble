// Copyright (c) 2010-2011 The Grumble Authors
// The use of this source code is goverened by a BSD-style
// license that can be found in the LICENSE-file.

package grumble

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-i2p/i2pkeys"
	"github.com/go-i2p/sam3"

	"github.com/eyedeekay/jumble/pkg/acl"
	"github.com/eyedeekay/jumble/pkg/ban"
	"github.com/eyedeekay/jumble/pkg/freezer"
	"github.com/eyedeekay/jumble/pkg/htmlfilter"
	"github.com/eyedeekay/jumble/pkg/logtarget"
	"github.com/eyedeekay/jumble/pkg/mumbleproto"
	"github.com/eyedeekay/jumble/pkg/serverconf"
	"github.com/eyedeekay/jumble/pkg/sessionpool"
	"github.com/eyedeekay/jumble/pkg/web"
	"github.com/golang/protobuf/proto"
)

// The default port a Murmur server listens on
const DefaultPort = 64738
const DefaultWebPort = 443
const UDPPacketSize = 1024

const LogOpsBeforeSync = 100
const CeltCompatBitstream = -2147483637
const (
	StateClientConnected = iota
	StateServerSentVersion
	StateClientSentVersion
	StateClientAuthenticated
	StateClientReady
	StateClientDead
)

type KeyValuePair struct {
	Key   string
	Value string
	Reset bool
}

func (s *Server) StreamListener() net.Listener {
	return s.streamListener
}

func (s *Server) TLSStreamListener() net.Listener {
	return s.tLSStreamListener
}

func (s *Server) DatagramConnection() net.PacketConn {
	return s.datagramConnection
}

// A Murmur server instance
type Server struct {
	Id int64

	streamListener     net.Listener
	tLSStreamListener  net.Listener
	datagramConnection net.PacketConn
	webListener        net.Listener
	TLSConfig          *tls.Config
	WebSocketListener  *web.Listener
	WebSocketTLSConfig *tls.Config
	WebServer          *http.Server
	//SAM                *sam3.SAM
	sam     *sam3.SAM
	SAM     *sam3.PrimarySession
	i2p     bool
	i2pkeys string

	bye     chan bool
	netwg   sync.WaitGroup
	running bool

	incoming       chan *Message
	voicebroadcast chan *VoiceBroadcast
	cfgUpdate      chan *KeyValuePair
	tempRemove     chan *Channel

	// Signals to the server that a client has been successfully
	// authenticated.
	clientAuthenticated chan *Client

	// Server configuration
	Config *serverconf.Config

	// Clients
	clients map[uint32]*Client

	// Host, host/port -> client mapping
	hmutex    sync.Mutex
	hclients  map[string][]*Client
	hpclients map[string]*Client

	// Codec information
	AlphaCodec       int32
	BetaCodec        int32
	PreferAlphaCodec bool
	Opus             bool

	// Channels
	Channels   map[int]*Channel
	nextChanId int

	// Users
	Users       map[uint32]*User
	UserCertMap map[string]*User
	UserNameMap map[string]*User
	nextUserId  uint32

	// Sessions
	pool *sessionpool.SessionPool

	// Freezer
	numLogOps int
	freezelog *freezer.Log

	// Bans
	banlock sync.RWMutex
	Bans    []ban.Ban

	// Logging
	*log.Logger
	datadir string
}

type clientLogForwarder struct {
	client *Client
	logger *log.Logger
}

func (lf clientLogForwarder) Write(incoming []byte) (int, error) {
	buf := new(bytes.Buffer)
	buf.WriteString(fmt.Sprintf("<%v:%v(%v)> ", lf.client.Session(), lf.client.ShownName(), lf.client.UserId()))
	buf.Write(incoming)
	lf.logger.Output(3, buf.String())
	return len(incoming), nil
}

// Allocate a new Murmur instance
func NewAnonServer(id int64, datadir string) (s *Server, err error) {
	s = new(Server)

	s.Id = id
	s.datadir = datadir
	s.i2pkeys = filepath.Join(datadir, "anonymous")

	s.Config = serverconf.New(nil)

	s.Users = make(map[uint32]*User)
	s.UserCertMap = make(map[string]*User)
	s.UserNameMap = make(map[string]*User)
	s.Users[0], err = NewUser(0, "SuperUser")
	s.UserNameMap["SuperUser"] = s.Users[0]
	s.nextUserId = 1

	s.Channels = make(map[int]*Channel)
	s.Channels[0] = NewChannel(0, "Root")
	s.nextChanId = 1
	s.i2p = true
	s.Config.Set("I2P", "true")

	s.Logger = log.New(logtarget.Default, fmt.Sprintf("[%v] ", s.Id), log.LstdFlags|log.Lmicroseconds)

	return
}

// Allocate a new Murmur instance
func NewServer(id int64, datadir string) (s *Server, err error) {
	s = new(Server)

	s.Id = id
	s.datadir = datadir

	s.Config = serverconf.New(nil)

	s.Users = make(map[uint32]*User)
	s.UserCertMap = make(map[string]*User)
	s.UserNameMap = make(map[string]*User)
	s.Users[0], err = NewUser(0, "SuperUser")
	s.UserNameMap["SuperUser"] = s.Users[0]
	s.nextUserId = 1

	s.Channels = make(map[int]*Channel)
	s.Channels[0] = NewChannel(0, "Root")
	s.nextChanId = 1
	s.i2p = false

	s.Logger = log.New(logtarget.Default, fmt.Sprintf("[%v] ", s.Id), log.LstdFlags|log.Lmicroseconds)

	return
}

// Debugf implements debug-level printing for Servers.
func (server *Server) Debugf(format string, v ...interface{}) {
	server.Printf(format, v...)
}

// RootChannel gets a pointer to the root channel
func (server *Server) RootChannel() *Channel {
	root, exists := server.Channels[0]
	if !exists {
		server.Fatalf("Not Root channel found for server")
	}
	return root
}

func (server *Server) setConfigPassword(key, password string) {
	saltBytes := make([]byte, 24)
	_, err := rand.Read(saltBytes)
	if err != nil {
		server.Fatalf("Unable to read from crypto/rand: %v", err)
	}

	salt := hex.EncodeToString(saltBytes)
	hasher := sha1.New()
	hasher.Write(saltBytes)
	hasher.Write([]byte(password))
	digest := hex.EncodeToString(hasher.Sum(nil))

	// Could be racy, but shouldn't really matter...
	val := "sha1$" + salt + "$" + digest
	server.Config.Set(key, val)

	if server.cfgUpdate != nil {
		server.cfgUpdate <- &KeyValuePair{Key: key, Value: val}
	}
}

// SetSuperUserPassword sets password as the new SuperUser password
func (server *Server) SetSuperUserPassword(password string) {
	server.setConfigPassword("SuperUserPassword", password)
}

// SetServerPassword sets password as the new Server password
func (server *Server) SetServerPassword(password string) {
	server.setConfigPassword("ServerPassword", password)
}

func (server *Server) checkConfigPassword(key, password string) bool {
	parts := strings.Split(server.Config.StringValue(key), "$")
	if len(parts) != 3 {
		return false
	}

	if len(parts[2]) == 0 {
		return false
	}

	var h hash.Hash
	switch parts[0] {
	case "sha1":
		h = sha1.New()
	default:
		// no such hash
		return false
	}

	// salt
	if len(parts[1]) > 0 {
		saltBytes, err := hex.DecodeString(parts[1])
		if err != nil {
			server.Fatalf("Unable to decode salt: %v", err)
		}
		h.Write(saltBytes)
	}

	// password
	h.Write([]byte(password))

	sum := hex.EncodeToString(h.Sum(nil))
	if parts[2] == sum {
		return true
	}

	return false
}

// CheckSuperUserPassword checks whether password matches the set SuperUser password.
func (server *Server) CheckSuperUserPassword(password string) bool {
	return server.checkConfigPassword("SuperUserPassword", password)
}

// CheckServerPassword checks whether password matches the set Server password.
func (server *Server) CheckServerPassword(password string) bool {
	return server.checkConfigPassword("ServerPassword", password)
}

func (server *Server) hasServerPassword() bool {
	return server.Config.StringValue("ServerPassword") != ""
}

// Called by the server to initiate a new client connection.
func (server *Server) handleIncomingClient(conn net.Conn) (err error) {
	client := new(Client)
	addr := conn.RemoteAddr()
	if addr == nil {
		err = errors.New("Unable to extract address for client.")
		return
	}

	client.lf = &clientLogForwarder{client, server.Logger}
	client.Logger = log.New(client.lf, "", 0)

	client.session = server.pool.Get()
	client.Printf("New connection: %v (%v)", conn.RemoteAddr(), client.Session())

	err = client.SetStreamAddr(addr)
	if err != nil {
		return
	}
	client.server = server
	client.conn = conn
	client.reader = bufio.NewReader(client.conn)

	client.state = StateClientConnected

	client.udprecv = make(chan []byte)
	client.voiceTargets = make(map[uint32]*VoiceTarget)

	client.user = nil

	// Extract user's cert hash
	// Only consider client certificates for direct connections, not WebSocket connections.
	// We do not support TLS-level client certificates for WebSocket client.
	if tlsconn, ok := client.conn.(*tls.Conn); ok {
		err = tlsconn.Handshake()
		if err != nil {
			client.Printf("TLS handshake failed: %v", err)
			client.Disconnect()
			return
		}

		state := tlsconn.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			hash := sha1.New()
			hash.Write(state.PeerCertificates[0].Raw)
			sum := hash.Sum(nil)
			client.certHash = hex.EncodeToString(sum)
		}

		// Check whether the client's cert hash is banned
		if server.IsCertHashBanned(client.CertHash()) {
			client.Printf("Certificate hash is banned")
			client.Disconnect()
			return
		}
	}

	// Launch network readers
	go client.tlsRecvLoop()
	go client.udpRecvLoop()

	return
}

// RemoveClient removes a disconnected client from the server's
// internal representation.
func (server *Server) RemoveClient(client *Client, kicked bool) {
	server.hmutex.Lock()
	host := GetIP(client.StreamAddr).String()
	oldclients := server.hclients[host]
	newclients := []*Client{}
	for _, hostclient := range oldclients {
		if hostclient != client {
			newclients = append(newclients, hostclient)
		}
	}
	server.hclients[host] = newclients
	if client.PacketAddr != nil {
		delete(server.hpclients, client.PacketAddr.String())
	}
	server.hmutex.Unlock()

	delete(server.clients, client.Session())
	server.pool.Reclaim(client.Session())

	// Remove client from channel
	channel := client.Channel
	if channel != nil {
		channel.RemoveClient(client)
	}

	// If the user was not kicked, broadcast a UserRemove message.
	// If the user is disconnect via a kick, the UserRemove message has already been sent
	// at this point.
	if !kicked && client.state > StateClientAuthenticated {
		err := server.broadcastProtoMessage(&mumbleproto.UserRemove{
			Session: proto.Uint32(client.Session()),
		})
		if err != nil {
			server.Panic("Unable to broadcast UserRemove message for disconnected client.")
		}
	}
}

// AddChannel adds a new channel to the server. Automatically assign it a channel ID.
func (server *Server) AddChannel(name string) (channel *Channel) {
	channel = NewChannel(server.nextChanId, name)
	server.Channels[channel.Id] = channel
	server.nextChanId += 1

	return
}

// RemoveChanel removes a channel from the server.
func (server *Server) RemoveChanel(channel *Channel) {
	if channel.Id == 0 {
		server.Printf("Attempted to remove root channel.")
		return
	}

	delete(server.Channels, channel.Id)
}

// Link two channels
func (server *Server) LinkChannels(channel *Channel, other *Channel) {
	channel.Links[other.Id] = other
	other.Links[channel.Id] = channel
}

// Unlink two channels
func (server *Server) UnlinkChannels(channel *Channel, other *Channel) {
	delete(channel.Links, other.Id)
	delete(other.Links, channel.Id)
}

// This is the synchronous handler goroutine.
// Important control channel messages are routed through this Goroutine
// to keep server state synchronized.
func (server *Server) handlerLoop() {
	regtick := time.Tick(time.Hour)
	for {
		select {
		// We're done. Stop the server's event handler
		case <-server.bye:
			return
		// Control channel messages
		case msg := <-server.incoming:
			client := msg.client
			server.handleIncomingMessage(client, msg)
		// Voice broadcast
		case vb := <-server.voicebroadcast:
			if vb.target == 0 { // Current channel
				channel := vb.client.Channel
				for _, client := range channel.clients {
					if client != vb.client {
						err := client.SendUDP(vb.buf)
						if err != nil {
							client.Panicf("Unable to send UDP: %v", err)
						}
					}
				}
			} else {
				target, ok := vb.client.voiceTargets[uint32(vb.target)]
				if !ok {
					continue
				}

				target.SendVoiceBroadcast(vb)
			}
		// Remove a temporary channel
		case tempChannel := <-server.tempRemove:
			if tempChannel.IsEmpty() {
				server.RemoveChannel(tempChannel)
			}
		// Finish client authentication. Send post-authentication
		// server info.
		case client := <-server.clientAuthenticated:
			server.finishAuthenticate(client)

		// Disk freeze config update
		case kvp := <-server.cfgUpdate:
			if !kvp.Reset {
				server.UpdateConfig(kvp.Key, kvp.Value)
			} else {
				server.ResetConfig(kvp.Key)
			}

		// Server registration update
		// Tick every hour + a minute offset based on the server id.
		case <-regtick:
			server.RegisterPublicServer()
		}

		// Check if its time to sync the server state and re-open the log
		if server.numLogOps >= LogOpsBeforeSync {
			server.Print("Writing full server snapshot to disk")
			err := server.FreezeToFile()
			if err != nil {
				server.Fatal(err)
			}
			server.numLogOps = 0
			server.Print("Wrote full server snapshot to disk")
		}
	}
}

// Handle an Authenticate protobuf message.  This is handled in a separate
// goroutine to allow for remote authenticators that are slow to respond.
//
// Once a user has been authenticated, it will ping the server's handler
// routine, which will call the finishAuthenticate method on Server which
// will send the channel tree, user list, etc. to the client.
func (server *Server) handleAuthenticate(client *Client, msg *Message) {
	// Is this message not an authenticate message? If not, discard it...
	if msg.kind != mumbleproto.MessageAuthenticate {
		client.Panic("Unexpected message. Expected Authenticate.")
		return
	}

	auth := &mumbleproto.Authenticate{}
	err := proto.Unmarshal(msg.buf, auth)
	if err != nil {
		client.Panic("Unable to unmarshal Authenticate message.")
		return
	}

	// Set access tokens. Clients can set their access tokens any time
	// by sending an Authenticate message with he contents of their new
	// access token list.
	client.tokens = auth.Tokens
	server.ClearCaches()

	if client.state >= StateClientAuthenticated {
		return
	}

	// Did we get a username?
	if auth.Username == nil || len(*auth.Username) == 0 {
		client.RejectAuth(mumbleproto.Reject_InvalidUsername, "Please specify a username to log in")
		return
	}

	client.Username = *auth.Username

	if client.Username == "SuperUser" {
		if auth.Password == nil {
			client.RejectAuth(mumbleproto.Reject_WrongUserPW, "")
			return
		} else {
			if server.CheckSuperUserPassword(*auth.Password) {
				ok := false
				client.user, ok = server.UserNameMap[client.Username]
				if !ok {
					client.RejectAuth(mumbleproto.Reject_InvalidUsername, "")
					return
				}
			} else {
				client.RejectAuth(mumbleproto.Reject_WrongUserPW, "")
				return
			}
		}
	} else {
		// First look up registration by name.
		user, exists := server.UserNameMap[client.Username]
		if exists {
			if client.HasCertificate() && user.CertHash == client.CertHash() {
				client.user = user
			} else {
				client.RejectAuth(mumbleproto.Reject_WrongUserPW, "Wrong certificate hash")
				return
			}
		}

		// Name matching didn't do.  Try matching by certificate.
		if client.user == nil && client.HasCertificate() {
			user, exists := server.UserCertMap[client.CertHash()]
			if exists {
				client.user = user
			}
		}
	}

	if client.user == nil && server.hasServerPassword() {
		if auth.Password == nil || !server.CheckServerPassword(*auth.Password) {
			client.RejectAuth(mumbleproto.Reject_WrongServerPW, "Invalid server password")
			return
		}
	}

	// Setup the cryptstate for the client.
	err = client.crypt.GenerateKey(client.CryptoMode)
	if err != nil {
		client.Panicf("%v", err)
		return
	}

	// Send CryptState information to the client so it can establish an UDP connection,
	// if it wishes.
	client.lastResync = time.Now().Unix()
	err = client.sendMessage(&mumbleproto.CryptSetup{
		Key:         client.crypt.Key,
		ClientNonce: client.crypt.DecryptIV,
		ServerNonce: client.crypt.EncryptIV,
	})
	if err != nil {
		client.Panicf("%v", err)
	}

	// Add codecs
	client.codecs = auth.CeltVersions
	client.opus = auth.GetOpus()

	client.state = StateClientAuthenticated
	server.clientAuthenticated <- client
}

// The last part of authentication runs in the server's synchronous handler.
func (server *Server) finishAuthenticate(client *Client) {
	// If the client succeeded in proving to the server that it should be granted
	// the credentials of a registered user, do some sanity checking to make sure
	// that user isn't already connected.
	//
	// If the user is already connected, try to check whether this new client is
	// connecting from the same IP address. If that's the case, disconnect the
	// previous client and let the new guy in.
	if client.user != nil {
		found := false
		for _, connectedClient := range server.clients {
			if connectedClient.UserId() == client.UserId() {
				found = true
				break
			}
		}
		// The user is already present on the server.
		if found {
			// todo(mkrautz): Do the address checking.
			client.RejectAuth(mumbleproto.Reject_UsernameInUse, "A client is already connected using those credentials.")
			return
		}

		// No, that user isn't already connected. Move along.
	}

	// Add the client to the connected list
	server.clients[client.Session()] = client

	// Warn clients without CELT support that they might not be able to talk to everyone else.
	if len(client.codecs) == 0 {
		client.codecs = []int32{CeltCompatBitstream}
		server.Printf("Client %v connected without CELT codecs. Faking compat bitstream.", client.Session())
		if server.Opus && !client.opus {
			client.sendMessage(&mumbleproto.TextMessage{
				Session: []uint32{client.Session()},
				Message: proto.String("<strong>WARNING:</strong> Your client doesn't support the CELT codec, you won't be able to talk to or hear most clients. Please make sure your client was built with CELT support."),
			})
		}
	}

	// First, check whether we need to tell the other connected
	// clients to switch to a codec so the new guy can actually speak.
	server.updateCodecVersions(client)

	client.sendChannelList()

	// Add the client to the host slice for its host address.
	host := GetIP(client.StreamAddr).String()
	server.hmutex.Lock()
	server.hclients[host] = append(server.hclients[host], client)
	server.hmutex.Unlock()

	channel := server.RootChannel()
	if client.IsRegistered() {
		lastChannel := server.Channels[client.user.LastChannelId]
		if lastChannel != nil {
			channel = lastChannel
		}
	}

	userstate := &mumbleproto.UserState{
		Session:   proto.Uint32(client.Session()),
		Name:      proto.String(client.ShownName()),
		ChannelId: proto.Uint32(uint32(channel.Id)),
	}

	if client.HasCertificate() {
		userstate.Hash = proto.String(client.CertHash())
	}

	if client.IsRegistered() {
		userstate.UserId = proto.Uint32(uint32(client.UserId()))

		if client.user.HasTexture() {
			// Does the client support blobs?
			if client.Version >= 0x10203 {
				userstate.TextureHash = client.user.TextureBlobHashBytes()
			} else {
				buf, err := BlobStore.Get(client.user.TextureBlob)
				if err != nil {
					server.Panicf("Blobstore error: %v", err.Error())
				}
				userstate.Texture = buf
			}
		}

		if client.user.HasComment() {
			// Does the client support blobs?
			if client.Version >= 0x10203 {
				userstate.CommentHash = client.user.CommentBlobHashBytes()
			} else {
				buf, err := BlobStore.Get(client.user.CommentBlob)
				if err != nil {
					server.Panicf("Blobstore error: %v", err.Error())
				}
				userstate.Comment = proto.String(string(buf))
			}
		}
	}

	server.userEnterChannel(client, channel, userstate)
	if err := server.broadcastProtoMessage(userstate); err != nil {
		// Server panic?
	}

	server.sendUserList(client)

	sync := &mumbleproto.ServerSync{}
	sync.Session = proto.Uint32(client.Session())
	sync.MaxBandwidth = proto.Uint32(server.Config.Uint32Value("MaxBandwidth"))
	sync.WelcomeText = proto.String(server.Config.StringValue("WelcomeText"))
	if client.IsSuperUser() {
		sync.Permissions = proto.Uint64(uint64(acl.AllPermissions))
	} else {
		// fixme(mkrautz): previously we calculated the user's
		// permissions and sent them to the client in here. This
		// code relied on our ACL cache, but that has been temporarily
		// thrown out because of our ACL handling code moving to its
		// own package.
		sync.Permissions = nil
	}
	if err := client.sendMessage(sync); err != nil {
		client.Panicf("%v", err)
		return
	}

	err := client.sendMessage(&mumbleproto.ServerConfig{
		AllowHtml:          proto.Bool(server.Config.BoolValue("AllowHTML")),
		MessageLength:      proto.Uint32(server.Config.Uint32Value("MaxTextMessageLength")),
		ImageMessageLength: proto.Uint32(server.Config.Uint32Value("MaxImageMessageLength")),
	})
	if err != nil {
		client.Panicf("%v", err)
		return
	}

	client.state = StateClientReady
	client.clientReady <- true
}

func (server *Server) updateCodecVersions(connecting *Client) {
	codecusers := map[int32]int{}
	var (
		winner     int32
		count      int
		users      int
		opus       int
		enableOpus bool
		txtMsg     *mumbleproto.TextMessage = &mumbleproto.TextMessage{
			Message: proto.String("<strong>WARNING:</strong> Your client doesn't support the Opus codec the server is switching to, you won't be able to talk or hear anyone. Please upgrade to a client with Opus support."),
		}
	)

	for _, client := range server.clients {
		users++
		if client.opus {
			opus++
		}
		for _, codec := range client.codecs {
			codecusers[codec] += 1
		}
	}

	for codec, users := range codecusers {
		if users > count {
			count = users
			winner = codec
		}
		if users == count && codec > winner {
			winner = codec
		}
	}

	var current int32
	if server.PreferAlphaCodec {
		current = server.AlphaCodec
	} else {
		current = server.BetaCodec
	}

	enableOpus = users == opus

	if winner != current {
		if winner == CeltCompatBitstream {
			server.PreferAlphaCodec = true
		} else {
			server.PreferAlphaCodec = !server.PreferAlphaCodec
		}

		if server.PreferAlphaCodec {
			server.AlphaCodec = winner
		} else {
			server.BetaCodec = winner
		}
	} else if server.Opus == enableOpus {
		if server.Opus && connecting != nil && !connecting.opus {
			txtMsg.Session = []uint32{connecting.Session()}
			connecting.sendMessage(txtMsg)
		}
		return
	}

	server.Opus = enableOpus

	err := server.broadcastProtoMessage(&mumbleproto.CodecVersion{
		Alpha:       proto.Int32(server.AlphaCodec),
		Beta:        proto.Int32(server.BetaCodec),
		PreferAlpha: proto.Bool(server.PreferAlphaCodec),
		Opus:        proto.Bool(server.Opus),
	})
	if err != nil {
		server.Printf("Unable to broadcast.")
		return
	}

	if server.Opus {
		for _, client := range server.clients {
			if !client.opus && client.state == StateClientReady {
				txtMsg.Session = []uint32{connecting.Session()}
				err := client.sendMessage(txtMsg)
				if err != nil {
					client.Panicf("%v", err)
				}
			}
		}
		if connecting != nil && !connecting.opus {
			txtMsg.Session = []uint32{connecting.Session()}
			connecting.sendMessage(txtMsg)
		}
	}

	server.Printf("CELT codec switch %#x %#x (PreferAlpha %v) (Opus %v)", uint32(server.AlphaCodec), uint32(server.BetaCodec), server.PreferAlphaCodec, server.Opus)
	return
}

func (server *Server) sendUserList(client *Client) {
	for _, connectedClient := range server.clients {
		if connectedClient.state != StateClientReady {
			continue
		}
		if connectedClient == client {
			continue
		}

		userstate := &mumbleproto.UserState{
			Session:   proto.Uint32(connectedClient.Session()),
			Name:      proto.String(connectedClient.ShownName()),
			ChannelId: proto.Uint32(uint32(connectedClient.Channel.Id)),
		}

		if connectedClient.HasCertificate() {
			userstate.Hash = proto.String(connectedClient.CertHash())
		}

		if connectedClient.IsRegistered() {
			userstate.UserId = proto.Uint32(uint32(connectedClient.UserId()))

			if connectedClient.user.HasTexture() {
				// Does the client support blobs?
				if client.Version >= 0x10203 {
					userstate.TextureHash = connectedClient.user.TextureBlobHashBytes()
				} else {
					buf, err := BlobStore.Get(connectedClient.user.TextureBlob)
					if err != nil {
						server.Panicf("Blobstore error: %v", err.Error())
					}
					userstate.Texture = buf
				}
			}

			if connectedClient.user.HasComment() {
				// Does the client support blobs?
				if client.Version >= 0x10203 {
					userstate.CommentHash = connectedClient.user.CommentBlobHashBytes()
				} else {
					buf, err := BlobStore.Get(connectedClient.user.CommentBlob)
					if err != nil {
						server.Panicf("Blobstore error: %v", err.Error())
					}
					userstate.Comment = proto.String(string(buf))
				}
			}
		}

		if connectedClient.Mute {
			userstate.Mute = proto.Bool(true)
		}
		if connectedClient.Suppress {
			userstate.Suppress = proto.Bool(true)
		}
		if connectedClient.SelfMute {
			userstate.SelfMute = proto.Bool(true)
		}
		if connectedClient.SelfDeaf {
			userstate.SelfDeaf = proto.Bool(true)
		}
		if connectedClient.PrioritySpeaker {
			userstate.PrioritySpeaker = proto.Bool(true)
		}
		if connectedClient.Recording {
			userstate.Recording = proto.Bool(true)
		}
		if connectedClient.PluginContext != nil || len(connectedClient.PluginContext) > 0 {
			userstate.PluginContext = connectedClient.PluginContext
		}
		if len(connectedClient.PluginIdentity) > 0 {
			userstate.PluginIdentity = proto.String(connectedClient.PluginIdentity)
		}

		err := client.sendMessage(userstate)
		if err != nil {
			// Server panic?
			continue
		}
	}
}

// Send a client its permissions for channel.
func (server *Server) sendClientPermissions(client *Client, channel *Channel) {
	// No caching for SuperUser
	if client.IsSuperUser() {
		return
	}

	// fixme(mkrautz): re-add when we have ACL caching
	return

	perm := acl.Permission(acl.NonePermission)
	client.sendMessage(&mumbleproto.PermissionQuery{
		ChannelId:   proto.Uint32(uint32(channel.Id)),
		Permissions: proto.Uint32(uint32(perm)),
	})
}

type ClientPredicate func(client *Client) bool

func (server *Server) broadcastProtoMessageWithPredicate(msg interface{}, clientcheck ClientPredicate) error {
	for _, client := range server.clients {
		if !clientcheck(client) {
			continue
		}
		if client.state < StateClientAuthenticated {
			continue
		}
		err := client.sendMessage(msg)
		if err != nil {
			return err
		}
	}

	return nil
}

func (server *Server) broadcastProtoMessage(msg interface{}) (err error) {
	err = server.broadcastProtoMessageWithPredicate(msg, func(client *Client) bool { return true })
	return
}

func (server *Server) handleIncomingMessage(client *Client, msg *Message) {
	switch msg.kind {
	case mumbleproto.MessageAuthenticate:
		server.handleAuthenticate(msg.client, msg)
	case mumbleproto.MessagePing:
		server.handlePingMessage(msg.client, msg)
	case mumbleproto.MessageChannelRemove:
		server.handleChannelRemoveMessage(msg.client, msg)
	case mumbleproto.MessageChannelState:
		server.handleChannelStateMessage(msg.client, msg)
	case mumbleproto.MessageUserState:
		server.handleUserStateMessage(msg.client, msg)
	case mumbleproto.MessageUserRemove:
		server.handleUserRemoveMessage(msg.client, msg)
	case mumbleproto.MessageBanList:
		server.handleBanListMessage(msg.client, msg)
	case mumbleproto.MessageTextMessage:
		server.handleTextMessage(msg.client, msg)
	case mumbleproto.MessageACL:
		server.handleAclMessage(msg.client, msg)
	case mumbleproto.MessageQueryUsers:
		server.handleQueryUsers(msg.client, msg)
	case mumbleproto.MessageCryptSetup:
		server.handleCryptSetup(msg.client, msg)
	case mumbleproto.MessageContextAction:
		server.Printf("MessageContextAction from client")
	case mumbleproto.MessageUserList:
		server.handleUserList(msg.client, msg)
	case mumbleproto.MessageVoiceTarget:
		server.handleVoiceTarget(msg.client, msg)
	case mumbleproto.MessagePermissionQuery:
		server.handlePermissionQuery(msg.client, msg)
	case mumbleproto.MessageUserStats:
		server.handleUserStatsMessage(msg.client, msg)
	case mumbleproto.MessageRequestBlob:
		server.handleRequestBlob(msg.client, msg)
	}
}

// Send the content of buf as a UDP packet to addr.
func (s *Server) SendDatagram(buf []byte, addr net.Addr) (err error) {
	_, err = s.DatagramConnection().WriteTo(buf, addr)
	return
}

// Listen for and handle UDP packets.
func (server *Server) udpListenLoop() {
	defer server.netwg.Done()

	buf := make([]byte, UDPPacketSize)
	for {
		nread, remote, err := server.DatagramConnection().ReadFrom(buf)
		if err != nil {
			if isTimeout(err) {
				continue
			} else {
				return
			}
		}

		var PacketAddr net.Addr
		var ok = false
		switch remote.(type) {
		case *net.UDPAddr:
			PacketAddr, ok = remote.(*net.UDPAddr)
			if !ok {
				server.Printf("No UDPAddr in read packet. Disabling UDP. (Windows?)")
				return
			}
		case *i2pkeys.I2PAddr:
			PacketAddr, ok = remote.(*i2pkeys.I2PAddr)
			if !ok {
				server.Printf("No *I2PAddr in read packet. Disabling UDP. (Windows?)")
				return
			}
		case i2pkeys.I2PAddr:
			PacketAddr, ok = remote.(i2pkeys.I2PAddr)
			if !ok {
				server.Printf("No I2PAddr in read packet. Disabling UDP. (Windows?)")
				return
			}
		}

		// Length 12 is for ping datagrams from the ConnectDialog.
		if nread == 12 {
			readbuf := bytes.NewBuffer(buf)
			var (
				tmp32 uint32
				rand  uint64
			)
			_ = binary.Read(readbuf, binary.BigEndian, &tmp32)
			_ = binary.Read(readbuf, binary.BigEndian, &rand)

			buffer := bytes.NewBuffer(make([]byte, 0, 24))
			_ = binary.Write(buffer, binary.BigEndian, uint32((1<<16)|(2<<8)|2))
			_ = binary.Write(buffer, binary.BigEndian, rand)
			_ = binary.Write(buffer, binary.BigEndian, uint32(len(server.clients)))
			_ = binary.Write(buffer, binary.BigEndian, server.Config.Uint32Value("MaxUsers"))
			_ = binary.Write(buffer, binary.BigEndian, server.Config.Uint32Value("MaxBandwidth"))

			err = server.SendDatagram(buffer.Bytes(), PacketAddr)
			if err != nil {
				return
			}

		} else {
			server.handleUdpPacket(PacketAddr, buf[0:nread])
		}
	}
}

func (server *Server) handleUdpPacket(PacketAddr net.Addr, buf []byte) {
	var match *Client
	plain := make([]byte, len(buf))

	// Determine which client sent the the packet.  First, we
	// check the map 'hpclients' in the server struct. It maps
	// a hort-post combination to a client.
	//
	// If we don't find any matches, we look in the 'hclients',
	// which maps a host address to a slice of clients.
	server.hmutex.Lock()
	defer server.hmutex.Unlock()
	client, ok := server.hpclients[PacketAddr.String()]
	if ok {
		err := client.crypt.Decrypt(plain, buf)
		if err != nil {
			client.Debugf("unable to decrypt incoming packet, requesting resync: %v", err)
			client.cryptResync()
			return
		}
		match = client
	} else {
		host := PacketAddr.String() //IP.String()
		hostclients := server.hclients[host]
		for _, client := range hostclients {
			err := client.crypt.Decrypt(plain[0:], buf)
			if err != nil {
				client.Debugf("unable to decrypt incoming packet, requesting resync: %v", err)
				client.cryptResync()
				return
			} else {
				match = client
			}
		}
		if match != nil {
			match.PacketAddr = PacketAddr
			server.hpclients[PacketAddr.String()] = match
		}
	}

	if match == nil {
		return
	}

	// Resize the plaintext slice now that we know
	// the true encryption overhead.
	plain = plain[:len(plain)-match.crypt.Overhead()]

	match.udp = true
	match.udprecv <- plain
}

// ClearCaches clears the Server's caches
func (server *Server) ClearCaches() {
	for _, client := range server.clients {
		client.ClearCaches()
	}
}

// Helper method for users entering new channels
func (server *Server) userEnterChannel(client *Client, channel *Channel, userstate *mumbleproto.UserState) {
	if client.Channel == channel {
		return
	}

	oldchan := client.Channel
	if oldchan != nil {
		oldchan.RemoveClient(client)
		if oldchan.IsTemporary() && oldchan.IsEmpty() {
			server.tempRemove <- oldchan
		}
	}
	channel.AddClient(client)

	server.ClearCaches()

	server.UpdateFrozenUserLastChannel(client)

	canspeak := acl.HasPermission(&channel.ACL, client, acl.SpeakPermission)
	if canspeak == client.Suppress {
		client.Suppress = !canspeak
		userstate.Suppress = proto.Bool(client.Suppress)
	}

	server.sendClientPermissions(client, channel)
	if channel.parent != nil {
		server.sendClientPermissions(client, channel.parent)
	}
}

// Register a client on the server.
func (s *Server) RegisterClient(client *Client) (uid uint32, err error) {
	// Increment nextUserId only if registration succeeded.
	defer func() {
		if err == nil {
			s.nextUserId += 1
		}
	}()

	user, err := NewUser(s.nextUserId, client.Username)
	if err != nil {
		return 0, err
	}

	// Grumble can only register users with certificates.
	if !client.HasCertificate() {
		return 0, errors.New("no cert hash")
	}

	user.Email = client.Email
	user.CertHash = client.CertHash()

	uid = s.nextUserId
	s.Users[uid] = user
	s.UserCertMap[client.CertHash()] = user
	s.UserNameMap[client.Username] = user

	return uid, nil
}

// RemoveRegistration removes a registered user.
func (s *Server) RemoveRegistration(uid uint32) (err error) {
	user, ok := s.Users[uid]
	if !ok {
		return errors.New("Unknown user ID")
	}

	// Remove from user maps
	delete(s.Users, uid)
	delete(s.UserCertMap, user.CertHash)
	delete(s.UserNameMap, user.Name)

	// Remove from groups and ACLs.
	s.removeRegisteredUserFromChannel(uid, s.RootChannel())

	return nil
}

// Remove references for user id uid from channel. Traverses subchannels.
func (s *Server) removeRegisteredUserFromChannel(uid uint32, channel *Channel) {

	newACL := []acl.ACL{}
	for _, chanacl := range channel.ACL.ACLs {
		if chanacl.UserId == int(uid) {
			continue
		}
		newACL = append(newACL, chanacl)
	}
	channel.ACL.ACLs = newACL

	for _, grp := range channel.ACL.Groups {
		if _, ok := grp.Add[int(uid)]; ok {
			delete(grp.Add, int(uid))
		}
		if _, ok := grp.Remove[int(uid)]; ok {
			delete(grp.Remove, int(uid))
		}
		if _, ok := grp.Temporary[int(uid)]; ok {
			delete(grp.Temporary, int(uid))
		}
	}

	for _, subChan := range channel.children {
		s.removeRegisteredUserFromChannel(uid, subChan)
	}
}

// RemoveChannel removes a channel
func (server *Server) RemoveChannel(channel *Channel) {
	// Can't remove root
	if channel == server.RootChannel() {
		return
	}

	// Remove all links
	for _, linkedChannel := range channel.Links {
		delete(linkedChannel.Links, channel.Id)
	}

	// Remove all subchannels
	for _, subChannel := range channel.children {
		server.RemoveChannel(subChannel)
	}

	// Remove all clients
	for _, client := range channel.clients {
		target := channel.parent
		for target.parent != nil && !acl.HasPermission(&target.ACL, client, acl.EnterPermission) {
			target = target.parent
		}

		userstate := &mumbleproto.UserState{}
		userstate.Session = proto.Uint32(client.Session())
		userstate.ChannelId = proto.Uint32(uint32(target.Id))
		server.userEnterChannel(client, target, userstate)
		if err := server.broadcastProtoMessage(userstate); err != nil {
			server.Panicf("%v", err)
		}
	}

	// Remove the channel itself
	parent := channel.parent
	delete(parent.children, channel.Id)
	delete(server.Channels, channel.Id)
	chanremove := &mumbleproto.ChannelRemove{
		ChannelId: proto.Uint32(uint32(channel.Id)),
	}
	if err := server.broadcastProtoMessage(chanremove); err != nil {
		server.Panicf("%v", err)
	}
}

// RemoveExpiredBans removes expired bans
func (server *Server) RemoveExpiredBans() {
	server.banlock.Lock()
	defer server.banlock.Unlock()

	newBans := []ban.Ban{}
	update := false
	for _, ban := range server.Bans {
		if !ban.IsExpired() {
			newBans = append(newBans, ban)
		} else {
			update = true
		}
	}

	if update {
		server.Bans = newBans
		server.UpdateFrozenBans(server.Bans)
	}
}

// IsConnectionBanned Is the incoming connection conn banned?
func (server *Server) IsConnectionBanned(conn net.Conn) bool {
	server.banlock.RLock()
	defer server.banlock.RUnlock()

	for _, ban := range server.Bans {
		switch conn.RemoteAddr().(type) {
		case *net.TCPAddr:
			addr := conn.RemoteAddr().(*net.TCPAddr)
			if ban.Match(addr.IP) && !ban.IsExpired() {
				return true
			}
		default:
			return false
		}
	}

	return false
}

// IsCertHashBanned Is the certificate hash banned?
func (server *Server) IsCertHashBanned(hash string) bool {
	server.banlock.RLock()
	defer server.banlock.RUnlock()

	for _, ban := range server.Bans {
		if ban.CertHash == hash && !ban.IsExpired() {
			return true
		}
	}

	return false
}

// Filter incoming text according to the server's current rules.
func (server *Server) FilterText(text string) (filtered string, err error) {
	options := &htmlfilter.Options{
		StripHTML:             !server.Config.BoolValue("AllowHTML"),
		MaxTextMessageLength:  server.Config.IntValue("MaxTextMessageLength"),
		MaxImageMessageLength: server.Config.IntValue("MaxImageMessageLength"),
	}
	return htmlfilter.Filter(text, options)
}

// The accept loop of the server.
func (server *Server) acceptLoop(listener net.Listener) {
	defer server.netwg.Done()

	for {
		// New client connected
		conn, err := listener.Accept()
		if err != nil {
			if isTimeout(err) {
				continue
			} else {
				return
			}
		}

		// Remove expired bans
		server.RemoveExpiredBans()

		// Is the client IP-banned?
		if server.IsConnectionBanned(conn) {
			server.Printf("Rejected client %v: Banned", conn.RemoteAddr())
			err := conn.Close()
			if err != nil {
				server.Printf("Unable to close connection: %v", err)
			}
			continue
		}

		// Create a new client connection from our *tls.Conn
		// which wraps net.TCPConn.
		err = server.handleIncomingClient(conn)
		if err != nil {
			server.Printf("Unable to handle new client: %v", err)
			continue
		}
	}
}

// The isTimeout function checks whether a
// network error is a timeout.
func isTimeout(err error) bool {
	if e, ok := err.(net.Error); ok {
		return e.Timeout()
	}
	return false
}

// Initialize the per-launch data
func (server *Server) initPerLaunchData() {
	server.pool = sessionpool.New()
	server.clients = make(map[uint32]*Client)
	server.hclients = make(map[string][]*Client)
	server.hpclients = make(map[string]*Client)

	server.bye = make(chan bool)
	server.incoming = make(chan *Message)
	server.voicebroadcast = make(chan *VoiceBroadcast)
	server.cfgUpdate = make(chan *KeyValuePair)
	server.tempRemove = make(chan *Channel, 1)
	server.clientAuthenticated = make(chan *Client)
}

// Clean per-launch data
func (server *Server) cleanPerLaunchData() {
	server.pool = nil
	server.clients = nil
	server.hclients = nil
	server.hpclients = nil

	server.bye = nil
	server.incoming = nil
	server.voicebroadcast = nil
	server.cfgUpdate = nil
	server.tempRemove = nil
	server.clientAuthenticated = nil
}

// Port returns the port the native server will listen on when it is
// started.
func (server *Server) Port() int {
	port := server.Config.IntValue("Port")
	if port == 0 {
		return DefaultPort + int(server.Id) - 1
	}
	return port
}

// ListenWebPort returns true if we should listen to the
// web port, otherwise false
func (server *Server) ListenWebPort() bool {
	return !server.Config.BoolValue("NoWebServer")
}

// WebPort returns the port the web server will listen on when it is
// started.
func (server *Server) WebPort() int {
	port := server.Config.IntValue("WebPort")
	if port == 0 {
		return DefaultWebPort + int(server.Id) - 1
	}
	return port
}

// CurrentPort returns the port the native server is currently listening
// on.  If called when the server is not running,
// this function returns -1.
func (server *Server) CurrentPort() int {
	if !server.running {
		return -1
	}
	if server.i2p {
		return DefaultPort
	}
	switch server.StreamListener().Addr().(type) {
	case *net.TCPAddr:
		StreamAddr := server.StreamListener().Addr().(*net.TCPAddr)
		return StreamAddr.Port
	default:
		return DefaultPort
	}
}

// HostAddress returns the host address the server will listen on when
// it is started. This must be an IP address, either IPv4
// or IPv6.
func (server *Server) HostAddress() string {
	host := server.Config.StringValue("Address")
	if host == "" {
		if server.i2p {
			return "127.0.0.1"
		}
		return "0.0.0.0"
	}
	return host
}

func (server *Server) ListenDatagram(addr net.Addr) (net.PacketConn, error) {
	switch addr.(type) {
	case i2pkeys.I2PAddr:
		log.Println("Listening on I2P Datagrams")
		return server.SAM.NewDatagramSubSession("mumble-i2p-"+sam3.RandString(), 0)
	default:
		return net.ListenUDP("udp", addr.(*net.UDPAddr))
	}
}

func (server *Server) ListenStream(addr net.Addr) (net.Listener, error) {
	switch addr.(type) {
	case i2pkeys.I2PAddr:
		log.Println("Listening on I2P Streaming")
		listener, err := server.SAM.NewStreamSubSessionWithPorts("mumble-stream-"+sam3.RandString(), "44445", "0")
		if err != nil {
			return nil, err
		}
		return listener.Listen()
	default:
		return net.ListenTCP("tcp", addr.(*net.TCPAddr))
	}
}

func (server *Server) ListenWeb(addr net.Addr) (net.Listener, error) {
	switch addr.(type) {
	case i2pkeys.I2PAddr:
		log.Println("Listening on I2P WebSocket")
		listener, err := server.SAM.NewStreamSubSession("mumble-web-" + sam3.RandString())
		if err != nil {
			return nil, err
		}
		return listener.Listen()
	default:
		return net.ListenTCP("tcp", addr.(*net.TCPAddr))
	}
}

func (server *Server) DatagramAddrString() string {
	if server.i2p {
		return server.SAM.Addr().Base32()
	}
	return server.DatagramAddr().String()
}

func (server *Server) DatagramAddr() net.Addr {
	if server.i2p {
		return server.SAM.Addr()
	}
	return &net.UDPAddr{IP: net.ParseIP(server.HostAddress()), Port: server.Port()}
}

func (server *Server) StreamAddrString() string {
	if server.i2p {
		return server.SAM.Addr().Base32()
	}
	return server.StreamAddr().String()
}

func (server *Server) TLSAddrString() string {
	if server.i2p {
		return server.SAM.Addr().Base32()
	}
	return server.StreamAddr().String()
}

func (server *Server) StreamAddr() net.Addr {
	if server.i2p {
		return server.SAM.Addr()
	}
	return &net.TCPAddr{IP: net.ParseIP(server.HostAddress()), Port: server.Port()}
}

func (server *Server) WebAddrString() string {
	if server.i2p {
		return server.SAM.Addr().Base32()
	}
	return server.WebAddr().String()
}

func (server *Server) WebAddr() net.Addr {
	if server.i2p {
		return server.SAM.Addr()
	}
	return &net.TCPAddr{IP: net.ParseIP(server.HostAddress()), Port: server.Port()}
}

// Start the server.
func (server *Server) Start() (err error) {
	if server.running {
		return errors.New("already running")
	}

	if server.i2p {
		server.sam, err = sam3.NewSAM("127.0.0.1:7656")
		if err != nil {
			return err
		}

		var tkeys i2pkeys.I2PKeys

		if _, err := os.Stat(server.i2pkeys + ".i2p.private"); os.IsNotExist(err) {
			f, err := os.Create(server.i2pkeys + ".i2p.private")
			if err != nil {
				return err
			}
			defer f.Close()
			tkeys, err = server.sam.NewKeys()
			if err != nil {
				return err
			}
			keys := &tkeys
			err = i2pkeys.StoreKeysIncompat(*keys, f)
			if err != nil {
				return err
			}
		} else {
			tkeys, err = i2pkeys.LoadKeys(server.i2pkeys + ".i2p.private")
			if err != nil {
				return err
			}
		}
		ioutil.WriteFile(server.i2pkeys+".i2p.public", []byte(tkeys.Addr().Base32()), 0644)
		if err != nil {
			return err
		}
		server.SAM, err = server.sam.NewPrimarySession("mumble-primary-"+sam3.RandString(), tkeys, sam3.Options_Medium)
		if err != nil {
			return err
		}
	}

	shouldListenWeb := server.ListenWebPort()

	// Setup our UDP listener
	server.datagramConnection, err = server.ListenDatagram(server.DatagramAddr())
	if err != nil {
		return err
	}

	// Set up our TCP connection
	server.streamListener, err = server.ListenStream(server.StreamAddr())
	if err != nil {
		return err
	}

	if shouldListenWeb {
		server.webListener, err = server.ListenWeb(server.WebAddr())
		if err != nil {
			return err
		}
	}

	// Wrap a TLS listener around the TCP connection
	certFn := filepath.Join(server.datadir, "cert.pem")
	keyFn := filepath.Join(server.datadir, "key.pem")
	cert, err := tls.LoadX509KeyPair(certFn, keyFn)
	if err != nil {
		return err
	}
	server.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequestClientCert,
		ServerName:   server.TLSAddrString(),
	}
	server.tLSStreamListener = tls.NewListener(server.StreamListener(), server.TLSConfig)

	if shouldListenWeb {
		// Create HTTP server and WebSocket "listener"
		server.WebSocketTLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.NoClientCert,
			NextProtos:   []string{"http/1.1"},
			ServerName:   server.WebAddrString(),
		}
		server.WebSocketListener = web.NewListener(server.WebAddr(), server.Logger)
		mux := http.NewServeMux()
		mux.Handle("/", server.WebSocketListener)
		server.WebServer = &http.Server{
			Addr:      server.WebAddrString(),
			Handler:   mux,
			TLSConfig: server.WebSocketTLSConfig,
			ErrorLog:  server.Logger,

			// Set sensible timeouts, in case no reverse proxy is in front of Grumble.
			// Non-conforming (or malicious) clients may otherwise block indefinitely and cause
			// file descriptors (or handles, depending on your OS) to leak and/or be exhausted
			ReadTimeout:  60 * time.Second,
			WriteTimeout: 80 * time.Second,
			IdleTimeout:  2 * time.Minute,
		}
		go func() {
			err := server.WebServer.ServeTLS(server.webListener, "", "")
			if err != http.ErrServerClosed {
				server.Fatalf("Fatal HTTP server error: %v", err)
			}
		}()

		server.Printf("Started: listening on %v and %v", server.StreamListener().Addr(), server.WebSocketListener.Addr())
	} else {
		server.Printf("Started: listening on %v", server.StreamListener().Addr())
	}

	server.running = true

	// Open a fresh freezer log
	err = server.openFreezeLog()
	if err != nil {
		server.Fatal(err)
	}

	// Reset the server's per-launch data to
	// a clean state.
	server.initPerLaunchData()

	// Launch the event handler goroutine
	go server.handlerLoop()

	// Add the three network receiver goroutines to the net waitgroup
	// and launch them.
	//
	// We use the waitgroup to provide a blocking Stop() method
	// for the servers. Each network goroutine defers a call to
	// netwg.Done(). In the Stop() we close all the connections
	// and call netwg.Wait() to wait for the goroutines to end.
	numWG := 2
	if shouldListenWeb {
		numWG++
	}

	server.netwg.Add(numWG)
	go server.udpListenLoop()
	go server.acceptLoop(server.TLSStreamListener())
	if shouldListenWeb {
		go server.acceptLoop(server.WebSocketListener)
	}

	// Schedule a server registration update (if needed)
	go func() {
		time.Sleep(1 * time.Minute)
		server.RegisterPublicServer()
	}()

	return nil
}

// Stop the server.
func (server *Server) Stop() (err error) {
	if !server.running {
		return errors.New("server not running")
	}

	// Stop the handler goroutine and disconnect all
	// clients
	server.bye <- true
	for _, client := range server.clients {
		client.Disconnect()
	}

	// Wait for the HTTP server to shutdown gracefully
	// A client could theoretically block the server from ever stopping by
	// never letting the HTTP connection go idle, so we give 15 seconds of grace time.
	// This does not apply to opened WebSockets, which were forcibly closed when
	// all clients were disconnected.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(15*time.Second))
	if server.ListenWebPort() {
		err = server.WebServer.Shutdown(ctx)
		cancel()
		if err == context.DeadlineExceeded {
			server.Println("Forcibly shutdown HTTP server while stopping")
		} else if err != nil {
			return err
		}

		err = server.WebSocketListener.Close()
		if err != nil {
			return err
		}
	}

	// Close the listeners
	err = server.TLSStreamListener().Close()
	if err != nil {
		return err
	}

	// Close the UDP connection
	err = server.DatagramConnection().Close()
	if err != nil {
		return err
	}

	// Since we'll (on some OSes) have to wait for the network
	// goroutines to end, we might as well use the time to store
	// a full server freeze to disk.
	err = server.FreezeToFile()
	if err != nil {
		server.Fatal(err)
	}

	// Wait for the three network receiver
	// goroutines end.
	server.netwg.Wait()

	server.cleanPerLaunchData()
	server.running = false
	server.Printf("Stopped")

	return nil
}

// Set will set a configuration value
func (server *Server) Set(key string, value string) {
	server.Config.Set(key, value)
}
