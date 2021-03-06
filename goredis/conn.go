package msgredis

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	ConnectTimeout    = 60e9
	ReadTimeout       = 60e9
	WriteTimeout      = 60e9
	DefaultBufferSize = 64

	RetryWaitSeconds = 2e9

	TypeError        = '-'
	TypeSimpleString = '+'
	TypeBulkString   = '$'
	TypeIntegers     = ':'
	TypeArrays       = '*'
)

var (
	ErrNil           = errors.New("nil data return")
	ErrBadType       = errors.New("invalid return type")
	ErrBadTcpConn    = errors.New("invalid tcp conn")
	ErrBadTerminator = errors.New("invalid terminator")
	ErrResponse      = errors.New("bad call")
	ErrNilPool       = errors.New("conn not belongs to any pool")
	ErrKeyNotExist   = errors.New(CommonErrPrefix + "key not exist")
	ErrBadArgs       = errors.New(CommonErrPrefix + "request args invalid")
	ErrEmptyDB       = errors.New(CommonErrPrefix + "empty db")

	CommonErrPrefix = "CommonError:"
)

//
type Conn struct {
	keepAlive      bool
	pipeCount      int
	lastActiveTime int64
	buffer         []byte
	conn           *net.TCPConn
	rb             *bufio.Reader
	wb             *bufio.Writer
	readTimeout    time.Duration
	writeTimeout   time.Duration
	pool           *Pool
}

func NewConn(conn *net.TCPConn, connectTimeout, readTimeout, writeTimeout time.Duration, keepAlive bool, pool *Pool) *Conn {
	return &Conn{
		conn:           conn,
		lastActiveTime: time.Now().Unix(),
		keepAlive:      keepAlive,
		buffer:         make([]byte, DefaultBufferSize),
		rb:             bufio.NewReader(conn),
		wb:             bufio.NewWriter(conn),
		readTimeout:    readTimeout,
		writeTimeout:   writeTimeout,
		pool:           pool,
	}
}

// connect with timeout
func Dial(address, password string, connectTimeout, readTimeout, writeTimeout time.Duration, keepAlive bool, pool *Pool) (*Conn, error) {
	c, e := net.DialTimeout("tcp", address, connectTimeout)
	if e != nil {
		return nil, e
	}
	if _, ok := c.(*net.TCPConn); !ok {
		return nil, ErrBadTcpConn
	}

	conn := NewConn(c.(*net.TCPConn), connectTimeout, readTimeout, writeTimeout, keepAlive, pool)
	if password != "" {
		if _, e := conn.AUTH(password); e != nil {
			return nil, e
		}
	}
	return conn, nil
}

func (c *Conn) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

// 连接无效了，无法从这里取一条新的连接，除非c里面有一个pool的指针
func (c *Conn) CallN(retry int, command string, args ...interface{}) (interface{}, error) {
	var ret interface{}
	var e error
	for i := 0; i < retry; i++ {
		ret, e = c.Call(command, args...)
		if e != nil && !strings.Contains(e.Error(), CommonErrPrefix) {
			time.Sleep(RetryWaitSeconds)
			// get a new conn from pool
			c.Close()
			if c.pool == nil {
				return nil, e
			}
			c = c.pool.Pop()
			if c == nil {
				return nil, e
			}
			continue
		}
	}
	return ret, e
}

// call redis command with request => response model
func (c *Conn) Call(command string, args ...interface{}) (interface{}, error) {
	c.lastActiveTime = time.Now().Unix()
	// start := time.Now()
	if c.pool != nil {
		c.pool.callMu.Lock()
		c.pool.CallNum++
		c.pool.callMu.Unlock()
	}
	var e error
	if c.writeTimeout > 0 {
		if e = c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); e != nil {
			return nil, e
		}
	}
	if e = c.writeRequest(command, args); e != nil {
		return nil, e
	}

	if e = c.wb.Flush(); e != nil {
		return nil, e
	}

	if c.readTimeout > 0 {
		if e = c.conn.SetReadDeadline(time.Now().Add(c.writeTimeout)); e != nil {
			return nil, e
		}
	}
	response, e := c.readResponse()
	if e != nil {
		return nil, e
	}
	// fmt.Println(command+" costs:", time.Now().Sub(start).String())
	return response, e
}

// write response
func (c *Conn) writeRequest(command string, args []interface{}) error {
	var e error
	if e = c.writeLen('*', 1+len(args)); e != nil {
		return e
	}

	if e = c.writeString(command); e != nil {
		return e
	}

	for _, arg := range args {
		if e != nil {
			return e
		}
		switch data := arg.(type) {
		case int:
			e = c.writeInt64(int64(data))
		case int64:
			e = c.writeInt64(data)
		case float64:
			e = c.writeFloat64(data)
		case string:
			e = c.writeString(data)
		case []byte:
			e = c.writeBytes(data)
		case bool:
			if data {
				e = c.writeString("1")
			} else {
				e = c.writeString("0")
			}
		case nil:
			e = c.writeString("")
		default:
			e = c.writeString(fmt.Sprintf("%v", data))
		}
	}
	return e
}

// reuse one buffer
func (c *Conn) writeLen(prefix byte, n int) error {
	pos := len(c.buffer) - 1
	c.buffer[pos] = '\n'
	pos--
	c.buffer[pos] = '\r'
	pos--

	for i := n; i != 0 && pos >= 0; i = i / 10 {
		c.buffer[pos] = byte(i%10 + '0')
		pos--
	}
	c.buffer[pos] = prefix
	_, e := c.wb.Write(c.buffer[pos:])
	if e != nil {
		return e
	}
	return nil
}

// write
func (c *Conn) writeBytes(b []byte) error {
	var e error
	if e = c.writeLen('$', len(b)); e != nil {
		return e
	}
	if _, e = c.wb.Write(b); e != nil {
		return e
	}
	if _, e = c.wb.WriteString("\r\n"); e != nil {
		return e
	}
	return nil
}

func (c *Conn) writeString(s string) error {
	var e error
	if e = c.writeLen('$', len(s)); e != nil {
		return e
	}
	if _, e = c.wb.WriteString(s); e != nil {
		return e
	}
	if _, e = c.wb.WriteString("\r\n"); e != nil {
		return e
	}
	return nil

}

func (c *Conn) writeFloat64(f float64) error {
	// Negative precision means "only as much as needed to be exact."
	return c.writeBytes(strconv.AppendFloat([]byte{}, f, 'g', -1, 64))
}

func (c *Conn) writeInt64(n int64) error {
	return c.writeBytes(strconv.AppendInt([]byte{}, n, 10))
}

// read
func (c *Conn) readResponse() (interface{}, error) {
	var e error
	p, e := c.readLine()
	if e != nil {
		return nil, e
	}
	resType := p[0]
	p = p[1:]
	switch resType {
	case TypeError:
		// 错误操作，非网络错误，不应该重建连接
		return nil, errors.New(CommonErrPrefix + string(p))
	case TypeIntegers:
		return c.parseInt(p)
	case TypeSimpleString:
		return p, nil
	case TypeBulkString:
		return c.parseBulkString(p)
	case TypeArrays:
		return c.parseArray(p)
	default:
	}
	return nil, errors.New(CommonErrPrefix + "Err type")
}

func (c *Conn) readLine() (b []byte, e error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Recovered in readLine", r)
			e = errors.New("readLine painc")
		}
	}()
	// var e error
	p, e := c.rb.ReadBytes('\n')
	if e != nil {
		return nil, e
	}

	i := len(p) - 2
	if i <= 0 {
		return nil, ErrBadTerminator
	}
	return p[:i], nil
}

func (c *Conn) parseInt(p []byte) (int64, error) {
	n, e := strconv.ParseInt(string(p), 10, 64)
	if e != nil {
		return 0, errors.New(CommonErrPrefix + e.Error())
	}
	return n, nil
}

func (c *Conn) parseBulkString(p []byte) (interface{}, error) {
	n, e := strconv.ParseInt(string(p), 10, 64)
	if e != nil {
		return []byte{}, errors.New(CommonErrPrefix + e.Error())
	}
	if n == -1 {
		return nil, nil
	}

	result := make([]byte, n+2)
	_, e = io.ReadFull(c.rb, result)
	return result[:n], e
}

func (c *Conn) parseArray(p []byte) ([]interface{}, error) {
	n, e := strconv.ParseInt(string(p), 10, 64)
	if e != nil {
		return nil, errors.New(CommonErrPrefix + e.Error())
	}

	if n == -1 {
		return nil, nil
	}

	result := make([]interface{}, n)
	var i int64
	for ; i < n; i++ {
		result[i], e = c.readResponse()
		if e != nil {
			return nil, e
		}
	}
	return result, nil
}

// pipeline与transactions没有用callN，失败没有重试
// pipeline
func (c *Conn) PipeSend(command string, args ...interface{}) error {
	c.pipeCount++
	return c.writeRequest(command, args)
}

func (c *Conn) PipeExec() ([]interface{}, error) {
	var e error
	if e = c.wb.Flush(); e != nil {
		return nil, e
	}
	n := c.pipeCount
	ret := make([]interface{}, c.pipeCount)
	c.pipeCount = 0
	for i := 0; i < n; i++ {
		ret[i], e = c.readResponse()
	}
	return ret, e
}

// Transactions
func (c *Conn) MULTI() error {
	ret, e := c.Call("MULTI")
	if e != nil {
		return e
	}
	if _, ok := ret.([]byte); !ok {
		return ErrBadType
	}
	r := ret.([]byte)
	if len(r) == 2 && r[0] == 'O' && r[1] == 'K' {
		return nil
	}
	return errors.New("invalid return:" + string(r))
}

func (c *Conn) TransSend(command string, args ...interface{}) error {
	ret, e := c.Call(command, args...)
	if e != nil {
		return e
	}
	if _, ok := ret.([]byte); !ok {
		return ErrBadType
	}
	r := ret.([]byte)
	if len(r) == 6 &&
		r[0] == 'Q' && r[1] == 'U' && r[2] == 'E' && r[3] == 'U' && r[4] == 'E' && r[5] == 'D' {
		return nil
	}
	return errors.New("invalid return:" + string(r))
}

func (c *Conn) TransExec() ([]interface{}, error) {
	ret, e := c.Call("EXEC")
	if e = c.wb.Flush(); e != nil {
		return nil, e
	}
	if ret == nil {
		// nil indicate transaction failed
		return nil, ErrNil
	}
	return ret.([]interface{}), e
}

func (c *Conn) Discard() error {
	ret, e := c.Call("DISCARD")
	if e != nil {
		return e
	}
	if _, ok := ret.([]byte); !ok {
		return ErrBadType
	}
	r := ret.([]byte)
	if len(r) == 2 && r[0] == 'O' && r[1] == 'K' {
		return nil
	}
	return errors.New("invalid return:" + string(r))
}

func (c *Conn) Watch(keys []string) error {
	args := make([]interface{}, len(keys))
	for i, key := range args {
		args[i] = key
	}
	ret, e := c.Call("WATCH", args...)
	if e != nil {
		return e
	}
	if _, ok := ret.([]byte); !ok {
		return ErrBadType
	}
	r := ret.([]byte)
	if len(r) == 2 && r[0] == 'O' && r[1] == 'K' {
		return nil
	}
	return errors.New("invalid return:" + string(r))
}
