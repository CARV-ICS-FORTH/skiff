package vxlan

import (
	"reflect"
	"testing"

	"plaid/pkg/packet"
)

func TestControlRoundTrip(t *testing.T) {
	c := control{typ: ctlHello, up: true, port: 8472, session: 0xdeadbeef, gen: 3, peerSess: 7, peerGen: 2,
		lid: 14, mtu: 4096, gidPrefix: 0xfe80000000000000, qps: []qpAddr{{qpn: 300, psn: 0x123456}, {qpn: 301, psn: 1}}}
	b := c.appendTo(nil, 1)
	if b[0] != packet.VXLANFlagControl {
		t.Fatalf("flags %#x: a control datagram has the control bit and not the I flag", b[0])
	}
	if _, err := packet.ParseVXLAN(b); err == nil {
		t.Fatal("ParseVXLAN took a control datagram: a plaidd without the transport would inject it")
	}
	got, err := parseControl(b[packet.VXLANHeaderLen:])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, c)
	}
	// NONE and DOWN carry no QPs.
	none := control{typ: ctlNone, port: 8472, session: 1}
	if got, err := parseControl(none.appendTo(nil, 1)[packet.VXLANHeaderLen:]); err != nil || got.typ != ctlNone || got.up {
		t.Fatalf("NONE: %+v %v", got, err)
	}
}

func TestControlRejectsWhatItCannotUse(t *testing.T) {
	good := control{typ: ctlHello, port: 8472, session: 9, gen: 1, lid: 3, mtu: 4096, qps: []qpAddr{{qpn: 1}}}
	payload := func(mod func(*control)) []byte {
		c := good
		c.qps = append([]qpAddr(nil), good.qps...)
		mod(&c)
		return c.appendTo(nil, 1)[packet.VXLANHeaderLen:]
	}
	cases := map[string][]byte{
		"short":           payload(func(*control) {})[:ctlFixedLen-1],
		"no port":         payload(func(c *control) { c.port = 0 }),
		"no session":      payload(func(c *control) { c.session = 0 }),
		"HELLO, no QPs":   payload(func(c *control) { c.qps = nil }),
		"HELLO, no gen":   payload(func(c *control) { c.gen = 0 }),
		"HELLO, no LID":   payload(func(c *control) { c.lid = 0 }),
		"too many QPs":    payload(func(c *control) { c.qps = make([]qpAddr, ctlMaxQPs+1) }),
		"unknown type":    payload(func(c *control) { c.typ = 9 }),
		"extra bytes":     append(payload(func(*control) {}), 0),
		"another version": append([]byte{2}, payload(func(*control) {})[1:]...),
	}
	for name, b := range cases {
		if _, err := parseControl(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
