package discovery

import (
	"net"
	"testing"
	"time"
)

func TestAddrManCooldown(t *testing.T) {
	orig := now
	defer func() { now = orig }()
	fake := time.Unix(1000, 0)
	now = func() time.Time { return fake }

	a := NewAddrMan()
	a.Add(net.ParseIP("1.2.3.4"), 41412, 0)
	const addr = "1.2.3.4:41412"

	if !a.NeedsTry(addr, dialCooldown) {
		t.Fatal("fresh addr should need a try")
	}
	a.MarkTried(addr)
	if a.NeedsTry(addr, dialCooldown) {
		t.Fatal("just-tried addr must be cooled down")
	}
	fake = fake.Add(dialCooldown + time.Second)
	if !a.NeedsTry(addr, dialCooldown) {
		t.Fatal("addr past cooldown should need a try again")
	}
}
