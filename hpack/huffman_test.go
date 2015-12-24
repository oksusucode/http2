package hpack

import (
	"encoding/hex"
	"testing"
)

var tests = []struct{ str, dump string }{
	{"www.example.com", "f1e3c2e5f23a6ba0ab90f4ff"},
	{"no-cache", "a8eb10649cbf"},
	{"custom-key", "25a849e95ba97d7f"},
	{"custom-value", "25a849e95bb8e8b4bf"},
	{"302", "6402"},
	{"private", "aec3771a4b"},
	{"Mon, 21 Oct 2013 20:13:21 GMT", "d07abe941054d444a8200595040b8166e082a62d1bff"},
	{"https://www.example.com", "9d29ad171863c78f0b97c8e9ae82ae43d3"},
	{"307", "640eff"},
	{"Mon, 21 Oct 2013 20:13:22 GMT", "d07abe941054d444a8200595040b8166e084a62d1bff"},
	{"gzip", "9bd9ab"},
	{"foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
		"94e7821dd7f2e6c7b335dfdfcd5b3960d5af27087f3672c1ab270fb5291f9587316065c003ed4ee5b1063d5007"},
}

func Test(t *testing.T) {
	t.Log(huffmanCodes[0])
}

func TestHuffmanEncode(t *testing.T) {
	for _, test := range tests {
		if got := hex.EncodeToString(HuffmanEncode([]byte{}, test.str)); test.dump != got {
			t.Errorf("encode: expected %v; got %v", test.dump, got)
		}
	}
}

func TestHuffmanDecode(t *testing.T) {
	for _, test := range tests {
		if x, err := hex.DecodeString(test.dump); err != nil {
			t.Error("error decoding hex:", err)
		} else if got := string(HuffmanDecode([]byte{}, x)); test.str != got {
			t.Errorf("decode: expected %v; got %v", test.str, got)
		}
	}
}

func TestHuffmanEncodeDecode(t *testing.T) {
	for _, test := range tests {
		if got := string(HuffmanDecode([]byte{}, HuffmanEncode([]byte{}, test.str))); test.str != got {
			t.Errorf("codec: expected %v; got %v", test.str, got)
		}
	}
}
