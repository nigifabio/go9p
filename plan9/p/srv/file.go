// Copyright 2009 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package srv

import "plan9/p"
import "sync"
import "syscall"
import "time"

// The FStatOp interface provides a single operation (Stat) that will be
// called before a file stat is sent back to the client. If implemented,
// the operation should update the data in the File struct.
type FStatOp interface {
	Stat() *p.Error;
}

// The FWstatOp interface provides a single operation (Wstat) that will be
// called when the client requests the File metadata to be modified. If
// implemented, the operation will be called when Twstat message is received.
// If not implemented, "permission denied" error will be sent back. If the
// operation returns an Error, the error is send back to the client.
type FWstatOp interface {
	Wstat(*p.Stat) *p.Error;
}

// If the FReadOp interface is implemented, the Read operation will be called
// to read from the file. If not implemented, "permission denied" error will
// be send back. The operation returns the number of bytes read, or the
// error occured while reading.
type FReadOp interface {
	Read(buf []byte, offset uint64) (int, *p.Error);
}

// If the FWriteOp interface is implemented, the Write operation will be called
// to write to the file. If not implemented, "permission denied" error will
// be send back. The operation returns the number of bytes written, or the
// error occured while writing.
type FWriteOp interface {
	Write(data []byte, offset uint64) (int, *p.Error);
}

// If the FCreateOp interface is implemented, the Create operation will be called
// when the client attempts to create a file in the File implementing the interface.
// If not implemented, "permission denied" error will be send back. If successful,
// the operation should call (*File)Add() to add the created file to the directory.
// The operation returns the created file, or the error occured while creating it.
type FCreateOp interface {
	Create(name string, perm uint32) (*File, *p.Error);
}

// If the FRemoveOp interface is implemented, the Remove operation will be called
// when the client attempts to create a file in the File implementing the interface.
// If not implemented, "permission denied" error will be send back.
// The operation returns nil if successful, or the error that occured while removing
// the file.
type FRemoveOp interface {
	Remove(*File) *p.Error;
}

// The File type represents a file (or directory) served by the file server.
type File struct {
	sync.Mutex;
	p.Stat;

	parent		*File;	// parent
	next, prev	*File;	// siblings, guarded by parent.Lock
	cfirst, clast	*File;	// children (if directory)
	ops		interface{};
}

type FFid struct {
	file		*File;
	nextchild	*File;	// used for readdir
}

// The Fsrv can be used to create file servers that serve
// simple trees of synthetic files.
type Fsrv struct {
	Srv;
	Root	*File;
}

var lock sync.Mutex
var qnext uint64
var Eexist = &p.Error{"file already exists", syscall.EEXIST}
var Enoent = &p.Error{"file not found", syscall.ENOENT}
var Enotempty = &p.Error{"directory not empty", syscall.EPERM}

// Creates a file server with root as root directory
func NewFileSrv(root *File) *Fsrv {
	srv := new(Fsrv);
	srv.Root = root;
	root.parent = root;	// make sure we can .. in root

	return srv;
}

// Initializes the fields of a file and add it to a directory.
// Returns nil if successful, or an error.
func (f *File) Add(dir *File, name string, uid p.User, gid p.Group, mode uint32, ops interface{}) *p.Error {

	lock.Lock();
	qpath := qnext;
	qnext++;
	lock.Unlock();

	f.Sqid.Type = uint8(mode >> 24);
	f.Sqid.Version = 0;
	f.Sqid.Path = qpath;
	f.Mode = mode;
	f.Atime = uint32(time.LocalTime().Seconds());
	f.Mtime = f.Atime;
	f.Length = 0;
	f.Name = name;
	if uid != nil {
		f.Uid = uid.Name();
		f.Nuid = uint32(uid.Id());
	} else {
		f.Uid = "none";
		f.Nuid = p.Nouid;
	}

	if gid != nil {
		f.Gid = gid.Name();
		f.Ngid = uint32(gid.Id());
	} else {
		f.Gid = "none";
		f.Ngid = p.Nouid;
	}

	f.Muid = "";
	f.Nmuid = p.Nouid;
	f.Ext = "";

	if dir != nil {
		f.parent = dir;
		dir.Lock();
		for p := dir.cfirst; p != nil; p = p.next {
			if name == p.Name {
				dir.Unlock();
				return Eexist;
			}
		}

		if dir.clast != nil {
			dir.clast.next = f
		} else {
			dir.cfirst = f
		}

		f.prev = dir.clast;
		f.next = nil;
		dir.clast = f;
		dir.Unlock();
	} else {
		f.parent = f
	}

	f.ops = ops;
	return nil;
}

// Removes a file from its parent directory.
func (f *File) Remove() {
	p := f.parent;

	p.Lock();
	if f.next != nil {
		f.next.prev = f.prev
	} else {
		p.clast = f.prev
	}

	if f.prev != nil {
		f.prev.next = f.next
	} else {
		p.cfirst = f.next
	}

	f.next = nil;
	f.prev = nil;
	p.Unlock();
}

// Looks for a file in a directory. Returns nil if the file is not found.
func (p *File) Find(name string) *File {
	var f *File;

	p.Lock();
	for f = p.cfirst; f != nil; f = f.next {
		if name == f.Name {
			break
		}
	}
	p.Unlock();
	return f;
}

// Checks if the specified user has permission to perform
// certain operation on a file. Perm contains one or more
// of p.DMREAD, p.DMWRITE, and p.DMEXEC.
func (f *File) CheckPerm(user p.User, perm uint32) bool {
	if user == nil {
		return false
	}

	perm &= 7;

	/* other permissions */
	fperm := f.Mode & 7;
	if (fperm & perm) == perm {
		return true
	}

	/* user permissions */
	if f.Uid == user.Name() || f.Nuid == uint32(user.Id()) {
		fperm |= (f.Mode >> 6) & 7
	}

	if (fperm & perm) == perm {
		return true
	}

	/* group permissions */
	groups := user.Groups();
	if groups != nil && len(groups) > 0 {
		for i := 0; i < len(groups); i++ {
			if f.Gid == groups[i].Name() || f.Ngid == uint32(groups[i].Id()) {
				fperm |= (f.Mode >> 3) & 7;
				break;
			}
		}
	}

	if (fperm & perm) == perm {
		return true
	}

	return false;
}

func (s *Fsrv) Attach(req *Req) {
	fid := new(FFid);
	fid.file = s.Root;
	req.Fid.Aux = fid;
	req.RespondRattach(&s.Root.Sqid);
}

func (*Fsrv) Walk(req *Req) {
	fid := req.Fid.Aux.(*FFid);
	tc := req.Tc;

	if req.Newfid.Aux == nil {
		req.Newfid.Aux = new(FFid)
	}

	nfid := req.Newfid.Aux.(*FFid);
	wqids := make([]p.Qid, len(tc.Wnames));
	i := 0;
	f := fid.file;
	for ; i < len(tc.Wnames); i++ {
		if tc.Wnames[i] == ".." {
			// handle dotdot
			f = f.parent;
			wqids[i] = f.Sqid;
			continue;
		}
		if (wqids[i].Type & p.QTDIR) > 0 {
			if !f.CheckPerm(req.Fid.User, p.DMEXEC) {
				break
			}
		}

		p := f.Find(tc.Wnames[i]);
		if p == nil {
			break
		}

		f = p;
		wqids[i] = f.Sqid;
	}

	if len(tc.Wnames) > 0 && i == 0 {
		req.RespondError(Enoent);
		return;
	}

	nfid.file = f;
	req.RespondRwalk(wqids[0:i]);
}

func mode2Perm(mode uint8) uint32 {
	var perm uint32 = 0;

	switch mode & 3 {
	case p.OREAD:
		perm = p.DMREAD
	case p.OWRITE:
		perm = p.DMWRITE
	case p.ORDWR:
		perm = p.DMREAD | p.DMWRITE
	}

	if (mode & p.OTRUNC) != 0 {
		perm |= p.DMWRITE
	}

	return perm;
}

func (*Fsrv) Open(req *Req) {
	fid := req.Fid.Aux.(*FFid);
	tc := req.Tc;

	if fid.file.CheckPerm(req.Fid.User, mode2Perm(tc.Mode)) {
		req.RespondError(Eperm);
		return;
	}

	req.RespondRopen(&fid.file.Sqid, 0);
}

func (*Fsrv) Create(req *Req) {
	fid := req.Fid.Aux.(*FFid);
	tc := req.Tc;

	dir := fid.file;
	if dir.CheckPerm(req.Fid.User, p.DMWRITE) {
		req.RespondError(Eperm);
		return;
	}

	if cop, ok := (dir.ops).(FCreateOp); ok {
		f, err := cop.Create(tc.Name, tc.Perm);
		if err != nil {
			req.RespondError(err)
		} else {
			fid.file = f;
			req.RespondRcreate(&fid.file.Sqid, 0);
		}
	} else {
		req.RespondError(Eperm)
	}
}

func (*Fsrv) Read(req *Req) {
	var n int;
	var err *p.Error;

	fid := req.Fid.Aux.(*FFid);
	f := fid.file;
	tc := req.Tc;
	rc := req.Rc;
	p.InitRread(rc, tc.Count);

	if f.Mode&p.DMDIR != 0 {
		// directory
		f.Lock();
		if tc.Offset == 0 {
			fid.nextchild = f.cfirst
		}

		b := rc.Data;
		for fid.nextchild != nil {
			sz := p.PackStat(&fid.nextchild.Stat, b, req.Conn.Dotu);
			if sz == 0 {
				break
			}

			b = b[sz:len(b)];
			fid.nextchild = fid.nextchild.next;
			n += sz;
		}
		f.Unlock();
	} else {
		// file
		if rop, ok := f.ops.(FReadOp); ok {
			n, err = rop.Read(rc.Data, tc.Offset);
			if err != nil {
				req.RespondError(err);
				return;
			}
		} else {
			req.RespondError(Eperm);
			return;
		}
	}

	p.SetRreadCount(rc, uint32(n));
	req.Respond();
}

func (*Fsrv) Write(req *Req) {
	fid := req.Fid.Aux.(*FFid);
	f := fid.file;
	tc := req.Tc;

	if wop, ok := (f.ops).(FWriteOp); ok {
		n, err := wop.Write(tc.Data, tc.Offset);
		if err != nil {
			req.RespondError(err)
		} else {
			req.RespondRwrite(uint32(n))
		}
	} else {
		req.RespondError(Eperm)
	}

}

func (*Fsrv) Clunk(req *Req)	{ req.RespondRclunk() }

func (*Fsrv) Remove(req *Req) {
	fid := req.Fid.Aux.(*FFid);
	f := fid.file;
	f.Lock();
	if f.cfirst != nil {
		f.Unlock();
		req.RespondError(Enotempty);
		return;
	}
	f.Unlock();

	if rop, ok := (f.parent.ops).(FRemoveOp); ok {
		err := rop.Remove(f);
		if err != nil {
			req.RespondError(err)
		} else {
			f.Remove();
			req.RespondRremove();
		}
	} else {
		req.RespondError(Eperm)
	}
}

func (*Fsrv) Stat(req *Req) {
	fid := req.Fid.Aux.(*FFid);
	f := fid.file;

	if sop, ok := (f.ops).(FStatOp); ok {
		err := sop.Stat();
		if err != nil {
			req.RespondError(err)
		} else {
			req.RespondRstat(&f.Stat)
		}
	} else {
		req.RespondRstat(&f.Stat)
	}
}

func (*Fsrv) Wstat(req *Req) {
	tc := req.Tc;
	fid := req.Fid.Aux.(*FFid);
	f := fid.file;

	if wop, ok := (f.ops).(FWstatOp); ok {
		err := wop.Wstat(&tc.Fstat);
		if err != nil {
			req.RespondError(err)
		} else {
			req.RespondRwstat()
		}
	} else {
		req.RespondError(Eperm)
	}
}