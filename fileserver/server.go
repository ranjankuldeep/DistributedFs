package fileserver

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ranjankuldeep/distributed_file_system/logs"
	"github.com/ranjankuldeep/distributed_file_system/p2p"
	"github.com/ranjankuldeep/distributed_file_system/store"
)

type FileServerOpts struct {
	ID                string
	StorageRoot       string
	PathTransformFunc store.PathTransformFunc
	Transport         p2p.Transport
	BootStrapNodes    []string
}
type FileServer struct {
	FileServerOpts
	store  *store.Store
	quitch chan struct{}

	peerLock sync.Mutex
	peers    map[string]p2p.Peer
}

// Message that is wired over.
type Message struct {
	Payload any
}

// Idenifier that payload will be of to store files.
type MessageStoreFile struct {
	ID   string
	Key  string
	Size int64
}

type MessageGetFile struct {
	ID  string
	Key string
}

func NewFileServer(opts FileServerOpts) *FileServer {
	storeOpts := store.StoreOpts{
		Root:              opts.StorageRoot,
		PathTransformFunc: opts.PathTransformFunc,
	}
	if len(opts.ID) == 0 {
		opts.ID = "1234"
	}
	return &FileServer{
		FileServerOpts: opts,
		store:          store.NewStore(storeOpts),
		quitch:         make(chan struct{}),
		peers:          make(map[string]p2p.Peer),
		peerLock:       sync.Mutex{},
	}
}

func (fs *FileServer) Start() error {
	if err := fs.Transport.ListenAndAccept(); err != nil {
		logs.Logger.Errorf("Failed to Listen")
		return err
	}
	fs.bootStrapNetwork() // Non Blocking
	fs.ReadLoop()         // Blocking
	return nil
}

func (fs *FileServer) Get(key string) (io.Reader, error) {
	if fs.store.Has(fs.ID, key) {
		logs.Logger.Infof("[%s] serving file (%s) from local disk\n", fs.Transport.Addr(), key)
		_, r, err := fs.store.Read(fs.ID, key)
		return r, err
	}

	logs.Logger.Infof("[%s] dont have file (%s) locally, fetching from network...\n", fs.Transport.Addr(), key)

	msg := Message{
		Payload: MessageGetFile{
			ID:  fs.ID,
			Key: key,
		},
	}

	if err := fs.BroadCast(&msg); err != nil {
		return nil, err
	}
	time.Sleep(time.Millisecond * 500)

	// Any peer over the network will start streaming the data.
	for _, peer := range fs.peers {
		// First read the file size so we can limit the amount of bytes that we read
		// from the connection, so it will not keep hanging.
		var fileSize int64
		binary.Read(peer, binary.LittleEndian, &fileSize)
		_, err := fs.store.Write(fs.ID, key, io.LimitReader(peer, fileSize))
		if err != nil {
			logs.Logger.Errorf("Unable to Write the Data Fetched Over the Network.")
		}
		logs.Logger.Infof("[%s] received (%d) bytes over the network from (%s)", fs.Transport.Addr(), fileSize, peer.RemoteAddr())
		peer.CloseStream()
	}

	_, r, err := fs.store.Read(fs.ID, key)
	if err != nil {
		logs.Logger.Errorf("Cannot read from the store %s", key)
	}
	return r, err
}

func (fs *FileServer) Store(key string, r io.Reader) error {
	var (
		fileBuffer = new(bytes.Buffer)
		tee        = io.TeeReader(r, fileBuffer)
	)
	// 1. SAVE THE FILE TO THIS DISK and get the size of the file (important for EOF on the network)
	size, err := fs.store.Write(fs.ID, key, tee)
	if err != nil {
		return err
	}
	msg := Message{
		Payload: MessageStoreFile{
			ID:   fs.ID,
			Key:  key,
			Size: size, // Specify the data size. (important)
		},
	}
	// 2. BROADCAST THE FILE TO ALL KNONW PEERS IN THE NETWORK.
	// Broadcast the key over the network
	if err := fs.BroadCast(&msg); err != nil {
		return err
	}

	time.Sleep(time.Millisecond * 1000)

	peers := []io.Writer{}
	for _, peer := range fs.peers {
		peers = append(peers, peer)
	}
	mw := io.MultiWriter(peers...)
	mw.Write([]byte{p2p.IncomingStream})
	if _, err := io.Copy(mw, fileBuffer); err != nil {
		logs.Logger.Errorf("Failed to stream data.")
		return err
	}
	logs.Logger.Infof("[%s] received and written (%d) bytes to disk\n", fs.Transport.Addr(), size)
	return nil
}

// Only Broadcasting the message.
func (fs *FileServer) BroadCast(msg *Message) error {
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(msg); err != nil {
		return err
	}
	for _, peer := range fs.peers {
		if err := peer.Send([]byte{p2p.IncomingMessage}); err != nil { // First send the incoming message after encoding.
			logs.Logger.Error(err)
			return err
		}
		if err := peer.Send(buf.Bytes()); err != nil {
			logs.Logger.Error(err)
			return err
		}
	}

	return nil
}

func (fs *FileServer) Stop() error {
	fs.quitch <- struct{}{}
	return nil
}

// Make sure that only a single go routine can change the
// peers map at a time
// map read is optimized for concurrent read but not map write.
func (s *FileServer) OnPeer(p p2p.Peer) error {
	s.peerLock.Lock()
	defer s.peerLock.Unlock()

	s.peers[p.RemoteAddr().String()] = p
	logs.Logger.Infof("connected with remote %s", p.RemoteAddr().String())
	return nil
}

func (fs *FileServer) ReadLoop() {
	// Keeps on looping for ever unitl quit. Blockin in nature.
	// Unless select it will again keeps on listenitng even if a channel has been hadled once.
	defer func() {
		logs.Logger.Info("File Server Stopped")
		fs.Transport.Close()
	}()
	for {
		select {
		case rpc := <-fs.Transport.Consume():
			var m Message // This is what recived over the wire.
			if err := gob.NewDecoder(bytes.NewReader(rpc.Payload)).Decode(&m); err != nil {
				logs.Logger.Errorf("Decoding Error %+v", err)

			}
			if err := fs.handleMessage(rpc.From, &m); err != nil {
				logs.Logger.Error(err)

			}
		case <-fs.quitch:
			logs.Logger.Info("User Quit Action")
			return
		}
	}
}

func (fs *FileServer) handleMessage(from string, msg *Message) error {
	switch v := msg.Payload.(type) {
	case MessageStoreFile:
		logs.Logger.Infof("Received key for Storing %+v\n", v)
		return fs.handleMessageStoreFile(from, &v)
	case MessageGetFile:
		return fs.handleMessageGetFile(from, v)
	}
	return nil
}

func (fs *FileServer) handleMessageStoreFile(from string, msg *MessageStoreFile) error {
	peer, ok := fs.peers[from]
	if !ok {
		return fmt.Errorf("peer (%s) could not be found in the peer list", from)
	}
	// A limit reader is necassary as over the network
	// when reading from the connection directly it will not send the EOF.
	// Which results in keep waiting until EOF.
	n, err := fs.store.Write(msg.ID, msg.Key, io.LimitReader(peer, msg.Size))
	if err != nil {
		return err
	}

	logs.Logger.Infof("[%s] written %d bytes to disk\n", fs.Transport.Addr(), n)
	peer.CloseStream() // Will trigger the read loop again for the connection.
	return nil
}

func (s *FileServer) handleMessageGetFile(from string, msg MessageGetFile) error {
	if !s.store.Has(msg.ID, msg.Key) {
		return fmt.Errorf("[%s] need to serve file (%s) but it does not exist on disk", s.Transport.Addr(), msg.Key)
	}

	fmt.Printf("[%s] serving file (%s) over the network\n", s.Transport.Addr(), msg.Key)

	fileSize, r, err := s.store.Read(msg.ID, msg.Key)
	if err != nil {
		return err
	}

	if rc, ok := r.(io.ReadCloser); ok {
		fmt.Println("closing readCloser")
		defer rc.Close()
	}

	peer, ok := s.peers[from]
	if !ok {
		return fmt.Errorf("peer %s not in map", from)
	}

	// First send the "incomingStream" byte to the peer and then we can send
	// the file size as an int64.
	peer.Send([]byte{p2p.IncomingStream})
	binary.Write(peer, binary.LittleEndian, fileSize)
	n, err := io.Copy(peer, r)
	if err != nil {
		return err
	}

	fmt.Printf("[%s] written (%d) bytes over the network to %s\n", s.Transport.Addr(), n, from)

	return nil
}

// Non blocking
func (fs *FileServer) bootStrapNetwork() error {
	for _, addr := range fs.BootStrapNodes {
		if len(addr) == 0 {
			continue
		}
		go func(addr string) {
			logs.Logger.Infof("attemting to connect with remote:%s", addr)
			if err := fs.Transport.Dial(addr); err != nil {
				logs.Logger.Errorf("Error BootStraping Network %v", err)
			}
		}(addr)
	}
	return nil
}

func init() {
	gob.Register(MessageStoreFile{})
	gob.Register(MessageGetFile{})
}
