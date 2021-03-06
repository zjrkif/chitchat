package chitchat

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

type ServerReadFunc func([]byte, ReadFuncer) error

type Server interface {
	Listen() error
	Cut() error
	CloseRemote(string) error
	RangeRemoteAddr() []string
	GetLocalAddr() string
	SetDeadLine(time.Duration, time.Duration)
	ErrChan() <-chan Errsocket
	Write(interface{}) error
}

type server struct {
	//unchangable data
	ipaddr   string
	readDDL  time.Duration
	writeDDL time.Duration
	//if delimiter is 0, then read until it's EOF
	delimiter byte
	readfunc  ServerReadFunc

	remoteMap *sync.Map

	//under protected data
	eDer
	eDerfunc eDinitfunc

	l          net.Listener
	cancelfunc context.CancelFunc //cancelfunc for cancel listener(CUT)

	//tempory used for readfunc
	currentConn net.Conn
	additional  interface{}
}

type hConnerServer struct {
	conn     net.Conn
	d        byte
	mu       *sync.Mutex
	readfunc ServerReadFunc
}

func NewServer(
	ipaddrsocket string,
	delim byte, readfunc ServerReadFunc, additional interface{}) Server {

	s := &server{
		ipaddr:    ipaddrsocket,
		readDDL:   0,
		writeDDL:  0,
		delimiter: delim,
		eDer: eDer{
			eU:     make(chan Errsocket),
			closed: false,
			mu:     new(sync.Mutex),
			pmu:    nil,
		},
		remoteMap:  new(sync.Map),
		readfunc:   readfunc,
		additional: additional,
	}
	s.eDerfunc = errDiversion(&s.eDer)
	return s
}

func (t *server) Listen() error { //Notifies the consumer when an error occurs ASYNCHRONOUSLY
	if t.readfunc == nil {
		return errors.New("read function is nil")
	}
	listener, err := net.Listen("tcp", t.ipaddr)
	if err != nil {
		return err
	}

	var ctx, cfunc = context.WithCancel(context.Background())
	t.cancelfunc = cfunc
	t.l = listener
	eC := make(chan Errsocket)
	go t.eDerfunc(eC)
	go handleListen(t, eC, ctx)
	return nil
}

func handleListen(s *server, eC chan Errsocket, ctx context.Context) {
	//fmt.Println("Start hL")
	//defer fmt.Println("->hL quit")
	defer close(eC)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			conn, err := s.l.Accept()
			if err != nil {
				s.mu.Lock()
				eC <- Errsocket{err, s.ipaddr}
				return
			}
			if s.readDDL != 0 {
				_ = conn.SetReadDeadline(time.Now().Add(s.readDDL))
			}
			if s.writeDDL != 0 {
				_ = conn.SetWriteDeadline(time.Now().Add(s.writeDDL))
			}

			cctx, childfunc := context.WithCancel(ctx) //TODO: if MAXLIVETIME is needed.
			s.remoteMap.Store(conn.RemoteAddr().String(), childfunc)

			ceC := make(chan Errsocket)
			go s.eDerfunc(ceC)
			go handleConnServer(&hConnerServer{
				conn:     conn,
				d:        s.delimiter,
				readfunc: s.readfunc,
				mu:       s.mu,
			}, ceC, cctx, s)
		}
	}
}

func handleConnServer(h *hConnerServer, eC chan Errsocket, ctx context.Context, s *server) {
	//fmt.Println("Start hCS:", h.conn.LocalAddr(), "->", h.conn.RemoteAddr())
	//defer fmt.Println("->hCS quit", h.conn.LocalAddr(), "->", h.conn.RemoteAddr())
	strReqChan := make(chan []byte)
	defer func() {
		err := h.conn.Close()
		<-strReqChan
		if err != nil {
			h.mu.Lock()
			eC <- Errsocket{err, h.conn.RemoteAddr().String()}
		}
		close(eC)
	}()

	go read(&reader{
		conn:       h.conn,
		d:          h.d,
		mu:         h.mu,
		strReqChan: strReqChan,
	}, eC)

	for {
		select {
		case <-ctx.Done(): //quit manually
			return
		case strReq, ok := <-strReqChan: //read a data slice successfully
			if !ok {
				return //EOF && d!=0
			}
			err := h.readfunc(strReq, &server{
				currentConn: h.conn,
				delimiter:   h.d,
				remoteMap:   s.remoteMap,
				additional:  s.additional,
			})
			if err != nil {
				h.mu.Lock()
				eC <- Errsocket{err, h.conn.RemoteAddr().String()}
			}
		}
	}
}

/*
will not wait for the rest of goroutines' error message.
make sure all connections has exited successfully before doing this
*/
func (t *server) Cut() error {
	t.mu.Lock()
	err := t.l.Close()
	if err != nil {
		//t.eU <- Errsocket{err, t.ipaddr}
		return err
	}
	t.closed = true
	close(t.eU)
	t.mu.Unlock()
	t.cancelfunc()
	return nil
}

func (t *server) CloseRemote(remoteAddr string) error {
	x, ok := t.remoteMap.Load(remoteAddr)
	if !ok {
		return errors.New(remoteAddr + " does not connected to this server")
	}
	x.(context.CancelFunc)()
	t.remoteMap.Delete(remoteAddr)
	return nil
}

func (t *server) Close() {
	remoteAddr := t.currentConn.RemoteAddr().String()
	x, ok := t.remoteMap.Load(remoteAddr)
	if !ok {
		t.eU <- Errsocket{errors.New("internal error"), t.ipaddr}
		return
	}
	x.(context.CancelFunc)()
	t.remoteMap.Delete(remoteAddr)
}

func (t *server) RangeRemoteAddr() []string {
	rtnstring := make([]string, 0)
	t.remoteMap.Range(
		func(key, value interface{}) bool {
			rtnstring = append(rtnstring, key.(string))
			return true
		})
	return rtnstring
}

func (t *server) SetDeadLine(rDDL time.Duration, wDDL time.Duration) {
	t.readDDL, t.writeDDL = rDDL, wDDL
}

func (t *server) ErrChan() <-chan Errsocket {
	return t.eU
}

func (t *server) GetRemoteAddr() string {
	if t.currentConn == nil {
		return ""
	}
	return t.currentConn.RemoteAddr().String()
}

func (t *server) GetLocalAddr() string {
	if t.currentConn == nil {
		return ""
	}
	return t.currentConn.LocalAddr().String()
}

func (t *server) GetConn() net.Conn {
	return t.currentConn
}

func (t *server) Addon() interface{} {
	return t.additional
}

func (t *server) Write(i interface{}) error {
	return writeFunc(t.currentConn, i, t.delimiter)
}
