// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Netlink sockets and messages

package syscall

import "unsafe"

// Round the length of a netlink message up to align it properly.
func nlmAlignOf(msglen int) int {
	return (msglen + NLMSG_ALIGNTO - 1) & ^(NLMSG_ALIGNTO - 1)
}

// Round the length of a netlink route attribute up to align it
// properly.
func rtaAlignOf(attrlen int) int {
	return (attrlen + RTA_ALIGNTO - 1) & ^(RTA_ALIGNTO - 1)
}

// NetlinkRouteRequest represents a request message to receive routing
// and link states from the kernel.
type NetlinkRouteRequest struct {
	Header NlMsghdr
	Data   RtGenmsg
}

func (rr *NetlinkRouteRequest) toWireFormat() []byte {
	b := make([]byte, rr.Header.Len)
	*(*uint32)(unsafe.Pointer(&b[0:4][0])) = rr.Header.Len
	*(*uint16)(unsafe.Pointer(&b[4:6][0])) = rr.Header.Type
	*(*uint16)(unsafe.Pointer(&b[6:8][0])) = rr.Header.Flags
	*(*uint32)(unsafe.Pointer(&b[8:12][0])) = rr.Header.Seq
	*(*uint32)(unsafe.Pointer(&b[12:16][0])) = rr.Header.Pid
	b[16] = byte(rr.Data.Family)
	return b
}

func newNetlinkRouteRequest(proto, seq, family int) []byte {
	rr := &NetlinkRouteRequest{}
	rr.Header.Len = uint32(NLMSG_HDRLEN + SizeofRtGenmsg)
	rr.Header.Type = uint16(proto)
	rr.Header.Flags = NLM_F_DUMP | NLM_F_REQUEST
	rr.Header.Seq = uint32(seq)
	rr.Data.Family = uint8(family)
	return rr.toWireFormat()
}

// NetlinkRIB returns routing information base, as known as RIB, which
// consists of network facility information, states and parameters.
func NetlinkRIB(proto, family int) ([]byte, error) {
	s, err := cloexecSocket(AF_NETLINK, SOCK_RAW, NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer Close(s)
	sa := &SockaddrNetlink{Family: AF_NETLINK}
	if err := Bind(s, sa); err != nil {
		return nil, err
	}
	wb := newNetlinkRouteRequest(proto, 1, family)
	if err := Sendto(s, wb, 0, sa); err != nil {
		return nil, err
	}
	lsa, err := Getsockname(s)
	if err != nil {
		return nil, err
	}
	lsanl, ok := lsa.(*SockaddrNetlink)
	if !ok {
		return nil, EINVAL
	}
	var tab []byte
	rbNew := make([]byte, Getpagesize())
done:
	for {
		rb := rbNew
		nr, _, err := Recvfrom(s, rb, 0)
		if err != nil {
			return nil, err
		}
		if nr < NLMSG_HDRLEN {
			return nil, EINVAL
		}
		rb = rb[:nr]
		tab = append(tab, rb...)
		msgs, err := ParseNetlinkMessage(rb)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if m.Header.Seq != 1 || m.Header.Pid != lsanl.Pid {
				return nil, EINVAL
			}
			if m.Header.Type == NLMSG_DONE {
				break done
			}
			if m.Header.Type == NLMSG_ERROR {
				return nil, EINVAL
			}
		}
	}
	return tab, nil
}

// NetlinkMessage represents a netlink message.
type NetlinkMessage struct {
	Header NlMsghdr
	Data   []byte
}

// ParseNetlinkMessage parses b as an array of netlink messages and
// returns the slice containing the NetlinkMessage structures.
func ParseNetlinkMessage(b []byte) ([]NetlinkMessage, error) {
	var msgs []NetlinkMessage
	for len(b) >= NLMSG_HDRLEN {
		h, dbuf, dlen, err := netlinkMessageHeaderAndData(b)
		if err != nil {
			return nil, err
		}
		m := NetlinkMessage{Header: *h, Data: dbuf[:int(h.Len)-NLMSG_HDRLEN]}
		msgs = append(msgs, m)
		b = b[dlen:]
	}
	return msgs, nil
}

func netlinkMessageHeaderAndData(b []byte) (*NlMsghdr, []byte, int, error) {
	h := (*NlMsghdr)(unsafe.Pointer(&b[0]))
	l := nlmAlignOf(int(h.Len))
	if int(h.Len) < NLMSG_HDRLEN || l > len(b) {
		return nil, nil, 0, EINVAL
	}
	return h, b[NLMSG_HDRLEN:], l, nil
}

// NetlinkRouteAttr represents a netlink route attribute.
type NetlinkRouteAttr struct {
	Attr  RtAttr
	Value []byte
}

// ParseNetlinkRouteAttr parses m's payload as an array of netlink
// route attributes and returns the slice containing the
// NetlinkRouteAttr structures.
func ParseNetlinkRouteAttr(m *NetlinkMessage) ([]NetlinkRouteAttr, error) {
	var b []byte
	switch m.Header.Type {
	case RTM_NEWLINK, RTM_DELLINK:
		b = m.Data[SizeofIfInfomsg:]
	case RTM_NEWADDR, RTM_DELADDR:
		b = m.Data[SizeofIfAddrmsg:]
	case RTM_NEWROUTE, RTM_DELROUTE:
		b = m.Data[SizeofRtMsg:]
	default:
		return nil, EINVAL
	}
	var attrs []NetlinkRouteAttr
	for len(b) >= SizeofRtAttr {
		a, vbuf, alen, err := netlinkRouteAttrAndValue(b)
		if err != nil {
			return nil, err
		}
		ra := NetlinkRouteAttr{Attr: *a, Value: vbuf[:int(a.Len)-SizeofRtAttr]}
		attrs = append(attrs, ra)
		b = b[alen:]
	}
	return attrs, nil
}

func netlinkRouteAttrAndValue(b []byte) (*RtAttr, []byte, int, error) {
	a := (*RtAttr)(unsafe.Pointer(&b[0]))
	if int(a.Len) < SizeofRtAttr || int(a.Len) > len(b) {
		return nil, nil, 0, EINVAL
	}
	return a, b[SizeofRtAttr:], rtaAlignOf(int(a.Len)), nil
}

func netlinkRecv(fd int, rb []byte, flags int) (int, error) {
	for {
		len, _, err := Recvfrom(fd, rb, flags)
		if len < 0 && err == EAGAIN {
			continue
		}
		if len < 0 {
			return len, err
		}
		if len == 0 {
			return 0, ENODATA
		}
		return len, nil
	}
}

// Receive common netlink messages, stop if NLMSG_DONE is received
func NetlinkRecvMsg(fd int, cb func([]NetlinkMessage)) error {
	var msgs []NetlinkMessage
	var doneFound = false
	for {
		var rb []byte
		// MSG_TRUNC tell the kernel to return true data size
		// nr should be the first skb size in recvive queue
		nr, err := netlinkRecv(fd, rb, MSG_PEEK|MSG_TRUNC)
		if err != nil {
			return err
		}
		if nr == 0 {
			break
		}
		rb = make([]byte, nr)
		nr, err = netlinkRecv(fd, rb, 0)
		if err != nil {
			return err
		}
		rb = rb[:nr]
		// a copy of ParseNetlinkMessage but with NLMSG_DONE parsed
		for len(rb) >= NLMSG_HDRLEN {
			h, dbuf, dlen, err := netlinkMessageHeaderAndData(rb)
			if err != nil {
				return err
			}
			if h.Type == NLMSG_DONE {
				doneFound = true
			}
			m := NetlinkMessage{Header: *h, Data: dbuf[:int(h.Len)-NLMSG_HDRLEN]}
			msgs = append(msgs, m)
			rb = rb[dlen:]
		}
		if cb != nil {
			cb(msgs)
		}
		if doneFound {
			return nil
		}
	}
	return nil
}
