package serve

import (
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"reasonix/internal/base/testenv"
	"reasonix/internal/contract/config"
)

// The phone doors stand on 127.0.0.2/3 in this file: the proxy's own door is
// 127.0.0.1, and the seam must leave that address to it.

func TestANamedProxyOpensItsOwnDoor(t *testing.T) {
	share := NewDeviceShare(fstest.MapFS{"index.html": {Data: []byte("x")}})
	share.addresses = func() []ShareAddress {
		return []ShareAddress{{Interface: "lo", IP: "127.0.0.2", Kind: AddressLAN}, {Interface: "lo", IP: "127.0.0.3", Kind: AddressLAN}}
	}
	share.publicOrigin = func() string { return proxyName }
	share.Attach(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(share.Close)

	if _, err := share.Open("127.0.0.2"); err != nil {
		t.Fatal(err)
	}
	st := share.Status()
	var proxyDoor *ShareDoor
	for i := range st.Doors {
		if st.Doors[i].Address == publicDoorIP {
			proxyDoor = &st.Doors[i]
		}
	}
	if proxyDoor == nil {
		t.Fatalf("doors = %+v, want the proxy's own door beside the phone's", st.Doors)
	}
	if proxyDoor.Kind != AddressLoopback {
		t.Fatalf("the proxy's door reads as %q, want loopback", proxyDoor.Kind)
	}
	for _, a := range st.Addresses {
		if a.IP == publicDoorIP {
			t.Fatal("the proxy's door sits in the picker no phone can dial")
		}
	}
	offer, err := share.Offer(publicDoorIP)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(offer.URL, proxyName+"/#pair=") {
		t.Fatalf("the proxy door's code names %q, want the public name", offer.URL)
	}
}

func TestUnnamedProxyOpensNoDoor(t *testing.T) {
	share := NewDeviceShare(nil)
	share.addresses = func() []ShareAddress { return []ShareAddress{{Interface: "lo", IP: "127.0.0.2", Kind: AddressLAN}} }
	share.Attach(http.NotFoundHandler())
	t.Cleanup(share.Close)

	if _, err := share.Open("127.0.0.2"); err != nil {
		t.Fatal(err)
	}
	for _, d := range share.Status().Doors {
		if d.Address == publicDoorIP {
			t.Fatal("a door nobody named is standing")
		}
	}
}

func TestANameBringsItsDoorAndClearingTakesItAway(t *testing.T) {
	t.Setenv("REASONIX_HOME", testenv.TempDir(t))
	share := NewDeviceShare(nil)
	share.addresses = func() []ShareAddress { return []ShareAddress{{Interface: "lo", IP: "127.0.0.2", Kind: AddressLAN}} }
	share.Attach(http.NotFoundHandler())
	// The name is read the way the host reads it: from the user config, which
	// the setting persists to before the door is asked for.
	share.SetPublicOrigin(func() string {
		loaded, err := config.Load()
		if err != nil {
			return ""
		}
		return loaded.SharePublicOrigin()
	})
	t.Cleanup(share.Close)

	if err := share.SetPublicOriginSetting(proxyName); err != nil {
		t.Fatal(err)
	}
	if _, err := share.Open("127.0.0.2"); err != nil {
		t.Fatal(err)
	}
	if doors := share.Status().Doors; len(doors) != 2 {
		t.Fatalf("doors = %+v, want the phone's and the proxy's", doors)
	}
	if err := share.SetPublicOriginSetting(""); err != nil {
		t.Fatal(err)
	}
	for _, d := range share.Status().Doors {
		if d.Address == publicDoorIP {
			t.Fatal("a cleared name left its door standing")
		}
	}
}
