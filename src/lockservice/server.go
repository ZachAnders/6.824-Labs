package lockservice

import "net"
import "net/rpc"
import "log"
import "sync"
import "fmt"
import "os"
import "io"
import "time"

type LockServer struct {
	mu    sync.Mutex
	l     net.Listener
	dead  bool // for test_test.go
	dying bool // for test_test.go

	am_primary bool   // am I the primary?
	backup     string // backup's port

	// for each lock name, is it locked?
	locks map[string]bool

	// For each lock, which RPC call locked it?
	lockers map[string]int64
	unlockers map[string]int64

	lockTime map[string]int64 //Make sure events don't happen out of order
	unlockTime map[string]int64
}

//
// server Lock RPC handler.
//
// you will have to modify this function
//
func (ls *LockServer) Lock(args *LockArgs, reply *LockReply) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	locked, _ := ls.locks[args.Lockname]
	lockerId, _ := ls.lockers[args.Lockname]
	unlockTime, _ := ls.unlockTime[args.Lockname]
	myId := "Secondary"
	if ls.am_primary {myId = "Primary"}
	// The first time a slave sees a non-forwarded packet, it should wait.
	// This way, clerks have the opportunity to failover
	//if !ls.am_primary && !args.IsForwarded && !ls.slaveWaited {
	//	time.Sleep(4 * time.Second) // Grace period for Clerks to failover
	//	ls.slaveWaited = true // We only need to wait once
	//}
	log.Printf("LOCK[%s] from: %d on %s at %d. Forwarded: %t", args.Lockname, args.CallerId, myId, args.Tstamp, args.IsForwarded)
	// If it's locked but a retransmission we don't care. If it's too early, that's bad though.
	if (locked && args.CallerId != lockerId) || args.Tstamp <= unlockTime {
		if args.CallerId == lockerId {
			// The caller is the one who originally unlocked it. Either a replay or a dupe.
			// Treat as a re-entrant lock I suppose
			reply.OK = true
		} else {
			reply.OK = false
		}
	} else {
		log.Printf("LOCKING[%s] on %s.", args.Lockname, myId)
		reply.OK = true
		// Both place the lock and set the CallerId
		ls.locks[args.Lockname] = true
		ls.lockers[args.Lockname] = args.CallerId
		ls.lockTime[args.Lockname] = args.Tstamp

		if ls.am_primary {
			var slaveReply LockReply
			args.IsForwarded = true
			// Forward request to backup
			ok := call(ls.backup, "LockServer.Lock", args, &slaveReply)
			if ok == false {
				ls.am_primary = false //Slave went down, stop forwarding requests to it
			}
		}
	}

	return nil
}

//
// server Unlock RPC handler.
//
func (ls *LockServer) Unlock(args *UnlockArgs, reply *UnlockReply) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	locked, _ := ls.locks[args.Lockname]
	lockerId, _ := ls.unlockers[args.Lockname]
	lockTime, _ := ls.lockTime[args.Lockname]
	myId := "Secondary"
	if ls.am_primary {myId = "Primary"}
	// The first time a slave sees a non-forwarded packet, it should wait.
	// This way, clerks have the opportunity to failover
	//if !args.IsForwarded && !ls.slaveWaited {
	//	time.Sleep(4 * time.Second) // Grace period for Clerks to failover
	//	ls.slaveWaited = true // We only need to wait once
	//}
	log.Printf("UNLOCK[%s] from: %d on %s at %d. Forwarded: %t", args.Lockname, args.CallerId, myId, args.Tstamp, args.IsForwarded)

	// Unlock is only valid if the lock was previously locked. If that is not the case, return an error
	// If (lock is locked and not a replay) and valid time stamp
	if (locked && args.CallerId != lockerId) && args.Tstamp >= lockTime {
		log.Printf("UNLOCKING[%s] on %s.", args.Lockname, myId)
		ls.locks[args.Lockname] = false
		ls.unlockers[args.Lockname] = args.CallerId
		ls.unlockTime[args.Lockname] = args.Tstamp
		if ls.am_primary {
			var slaveReply UnlockReply
			// Forward request to backup
			args.IsForwarded = true
			ok := call(ls.backup, "LockServer.Unlock", args, &slaveReply)
			if ok == false {
				ls.am_primary = false //Slave went down, stop forwarding requests to it
			}
		}
		reply.OK = true
	} else {
		if args.CallerId == lockerId {
			// The caller is the one who originally unlocked it. Either a replay or a dupe.
			// Treat as a re-entrant lock I suppose
			reply.OK = true
		} else {
			reply.OK = false
		}
	}

	return nil
}

//
// tell the server to shut itself down.
// for testing.
// please don't change this.
//
func (ls *LockServer) kill() {
	ls.dead = true
	ls.l.Close()
}

//
// hack to allow test_test.go to have primary process
// an RPC but not send a reply. can't use the shutdown()
// trick b/c that causes client to immediately get an
// error and send to backup before primary does.
// please don't change anything to do with DeafConn.
//
type DeafConn struct {
	c io.ReadWriteCloser
}

func (dc DeafConn) Write(p []byte) (n int, err error) {
	return len(p), nil
}
func (dc DeafConn) Close() error {
	return dc.c.Close()
}
func (dc DeafConn) Read(p []byte) (n int, err error) {
	return dc.c.Read(p)
}

func StartServer(primary string, backup string, am_primary bool) *LockServer {
	ls := new(LockServer)
	ls.backup = backup
	ls.am_primary = am_primary
	ls.locks = map[string]bool{}
	ls.lockers = map[string]int64{}
	ls.unlockers = map[string]int64{}
	ls.lockTime = map[string]int64{}
	ls.unlockTime = map[string]int64{}

	// Your initialization code here.

	me := ""
	if am_primary {
		me = primary
	} else {
		me = backup
	}

	// tell net/rpc about our RPC server and handlers.
	rpcs := rpc.NewServer()
	rpcs.Register(ls)

	// prepare to receive connections from clients.
	// change "unix" to "tcp" to use over a network.
	os.Remove(me) // only needed for "unix"
	l, e := net.Listen("unix", me)
	if e != nil {
		log.Fatal("listen error: ", e)
	}
	ls.l = l

	// please don't change any of the following code,
	// or do anything to subvert it.

	// create a thread to accept RPC connections from clients.
	go func() {
		for ls.dead == false {
			conn, err := ls.l.Accept()
			if err == nil && ls.dead == false {
				if ls.dying {
					// process the request but force discard of reply.

					// without this the connection is never closed,
					// b/c ServeConn() is waiting for more requests.
					// test_test.go depends on this two seconds.
					go func() {
						time.Sleep(2 * time.Second)
						conn.Close()
					}()
					ls.l.Close()

					// this object has the type ServeConn expects,
					// but discards writes (i.e. discards the RPC reply).
					deaf_conn := DeafConn{c: conn}

					rpcs.ServeConn(deaf_conn)

					ls.dead = true
				} else {
					go rpcs.ServeConn(conn)
				}
			} else if err == nil {
				conn.Close()
			}
			if err != nil && ls.dead == false {
				fmt.Printf("LockServer(%v) accept: %v\n", me, err.Error())
				ls.kill()
			}
		}
	}()

	return ls
}
