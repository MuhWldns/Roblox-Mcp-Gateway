package bridgeproto

import "testing"

func FuzzDecode(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"version":1,"type":"heartbeat","device_id":"dev_1"}`),
		[]byte(`{"version":1,"type":"request","gateway_request_id":"gw_1","device_id":"dev_1","payload":{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}}`),
		[]byte(`{"version":2,"type":"future-message","device_id":"dev_1","unknown":true}`),
		[]byte(`{"version":1,"type":"request"`),
		{},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		const maxBytes = 4096
		_, _ = Decode(data, Limits{MaxPayloadBytes: maxBytes})
	})
}
