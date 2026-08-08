package keepalived

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	got := parse(`
global_defs {
    router_id lb-svr1
}
vrrp_instance VI_1 {
    state MASTER
    interface eth0
    virtual_router_id 51
    priority 255
    advert_int 1
    authentication {
        auth_type PASS
        auth_pass secret
    }
    virtual_ipaddress {
        192.168.2.154/24
    }
}
vrrp_instance VI_2 {
    virtual_router_id 52
    priority 100
    unicast_peer {
        10.0.0.2
    }
    virtual_ipaddress {
        10.0.0.100
        10.0.0.101/32
    }
}
`)
	want := []Instance{
		{Name: "VI_1", VRID: 51, Priority: 255, VIPs: []string{"192.168.2.154"}},
		{Name: "VI_2", VRID: 52, Priority: 100, VIPs: []string{"10.0.0.100", "10.0.0.101"}, Unicast: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseFileMissing(t *testing.T) {
	inst, err := ParseFile("/nonexistent/keepalived.conf")
	if inst != nil || err != nil {
		t.Fatalf("missing file should be (nil, nil), got %v %v", inst, err)
	}
}
