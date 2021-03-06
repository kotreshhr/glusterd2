package sunrpc

import (
	"expvar"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"os"
	"path"
	"sync"
	"syscall"

	"github.com/gluster/glusterd2/glusterd2/pmap"
	"github.com/gluster/glusterd2/pkg/sunrpc"

	"github.com/cockroachdb/cmux"
	log "github.com/sirupsen/logrus"
	config "github.com/spf13/viper"
)

const gd2SocketFile = "glusterd2.socket"

var (
	// metrics
	clientCount = expvar.NewInt("sunrpc_clients_connected")
)

var programsList = []sunrpc.Program{
	newGfHandshake(),
	newGfDump(),
	pmap.NewGfPortmap(),
}

// SunRPC implements a suture service
type SunRPC struct {
	tcpListener   net.Listener
	tcpStopCh     chan struct{}
	unixListener  net.Listener
	unixStopCh    chan struct{}
	notifyCloseCh chan io.ReadWriteCloser
	lockFileFd    int
}

// clientsList is global as it needs to be accessed by RPC procedures
// that notify connected clients.
var clientsList = struct {
	sync.RWMutex
	c map[net.Conn]struct{}
}{
	// This map is used as a set. Values are not consumed.
	c: make(map[net.Conn]struct{}),
}

// NewMuxed returns a SunRPC server configured to listen on a CMux multiplexed connection
func NewMuxed(m cmux.CMux) *SunRPC {

	f := path.Join(config.GetString("rundir"), gd2SocketFile)
	gd2LockFile := f + ".lock"
	fd, err := syscall.Open(gd2LockFile,
		syscall.O_CREAT|syscall.O_WRONLY|syscall.O_CLOEXEC, 0666)
	if err != nil {
		log.WithError(err).WithField("lockfile", gd2LockFile).Fatal("failed to open lock file")
	}

	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		log.WithError(err).WithField("socket", gd2SocketFile).Fatal("failed to get lock")
	}

	err = os.Remove(f)
	if err != nil && !os.IsNotExist(err) {
		log.WithError(err).WithField("socket", gd2SocketFile).Fatal("failed to cleanup socket file")
	}

	uL, err := net.Listen("unix", f)
	if err != nil {
		// FIXME: Remove fatal and bubble up error to main()
		log.WithError(err).WithField("socket", gd2SocketFile).Fatal("failed to listen")
	}
	// This cleanup happens for process shutdown on SIGTERM/SIGINT but not on SIGKILL.
	uL.(*net.UnixListener).SetUnlinkOnClose(true)

	srv := &SunRPC{
		tcpListener:   m.Match(sunrpc.CmuxMatcher()),
		unixListener:  uL,
		tcpStopCh:     make(chan struct{}),
		unixStopCh:    make(chan struct{}),
		notifyCloseCh: make(chan io.ReadWriteCloser, 10),
		lockFileFd:    fd,
	}

	for _, prog := range programsList {
		err := registerProcedures(prog)
		if err != nil {
			log.WithError(err).WithField("program", prog.Name()).Error("could not register SunRPC program")
			return nil
		}
	}

	return srv
}

// pruneConn detects client disconnections and prunes clients list
func (s *SunRPC) pruneConn() {
	logger := log.WithField("server", "sunrpc")
	for rwc := range s.notifyCloseCh {
		conn := rwc.(net.Conn)
		logger.WithField("address", conn.RemoteAddr().String()).Info("client disconnected")

		clientsList.Lock()
		delete(clientsList.c, conn)
		pmap.ProcessDisconnect(conn)
		clientsList.Unlock()

		clientCount.Add(-1)
	}
}

func (s *SunRPC) acceptLoop(stopCh chan struct{}, l net.Listener, wg *sync.WaitGroup) {
	defer wg.Done()

	var ltype string
	switch l.(type) {
	case *net.UnixListener:
		ltype = "unix"
	default:
		ltype = "tcp"
	}
	logger := log.WithFields(log.Fields{
		"server":    "sunrpc",
		"transport": ltype})
	logger.WithField("address", l.Addr().String()).Info("started server")

	for {
		select {
		case <-stopCh:
			logger.Debug("stopped accepting new connections")
			return
		default:
		}

		conn, err := l.Accept()
		if err != nil {
			continue
		}

		logger.WithField("address", conn.RemoteAddr().String()).Info("client connected")
		clientCount.Add(1)
		clientsList.Lock()
		clientsList.c[conn] = struct{}{}
		clientsList.Unlock()

		// Create one rpc.Server instance per client. This is a
		// workaround to allow RPC programs to access underlying
		// net.Conn object and has minimal overhead. See:
		// https://groups.google.com/d/msg/golang-nuts/Gt-1ikXovCA/aK8r9MAftDQJ
		server := rpc.NewServer()

		for _, p := range programsList {
			if v, ok := p.(Conn); ok {
				v.SetConn(conn)
			}
			// server.Register() throws some benign but very
			// annoying log messages complaining about signatures
			// of methods. These logs can be safely ignored. See:
			// https://github.com/golang/go/issues/19957
			if err := server.Register(p); err != nil {
				panic(fmt.Sprintf("rpc.Register failed: %s", err.Error()))
			}
		}

		// For each session, start two goroutines:
		//   1) Run the rpc server, and when the server terminates, close sessionCh to terminate goroutine#2
		//   2) Wait on sessionCh and stopCh, close the session and return if either comes. session.Close should
		//      terminate #1
		session := sunrpc.NewServerCodec(conn, s.notifyCloseCh)
		sessionCh := make(chan struct{})
		go func() {
			defer close(sessionCh)
			server.ServeCodec(session)
		}()
		go func() {
			select {
			case <-stopCh:
				session.Close()
				return
			case <-sessionCh:
				session.Close()
				return
			}
		}()
	}
}

// Serve will start accepting Sun RPC client connections on the listener
// provided.
func (s *SunRPC) Serve() {
	// FIXME: This goroutine leaks, the fix however makes code look complex.
	// We will need two separate servers once we decide that local daemons
	// only communicate over Unix sockets. Deferring this until then.
	go s.pruneConn()

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go s.acceptLoop(s.tcpStopCh, s.tcpListener, wg)

	wg.Add(1)
	go s.acceptLoop(s.unixStopCh, s.unixListener, wg)

	wg.Wait()
}

// Stop stops the SunRPC server
func (s *SunRPC) Stop() {
	close(s.tcpStopCh)
	close(s.unixStopCh)

	// Close UDS listener; cmux should take care of the TCP one.
	s.unixListener.Close()
	syscall.Close(s.lockFileFd)
}
