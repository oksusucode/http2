package hpack

const headerEntryOverhead = 32

func HeaderFieldSize(name, value string) uint32 {
	return uint32(len(name) + len(value) + headerEntryOverhead)
}

type headerField struct {
	name, value string
}

type headerTable struct {
	data          []headerField
	size, maxSize uint32
}

func (t *headerTable) setMaxSize(max uint32) bool {
	if max == t.maxSize {
		return false
	}
	t.maxSize = max
	if max == 0 {
		t.data = t.data[:0]
		t.size = 0
	} else {
		data := t.data
		for t.size > t.maxSize {
			x := t.data[0]
			t.size -= HeaderFieldSize(x.name, x.value)
			t.data = t.data[1:]
		}
		if len(t.data) != len(data) {
			copy(data, t.data)
			t.data = data[:len(t.data)]
		}
	}
	return true
}

func (t *headerTable) add(name, value string) bool {
	hsize := HeaderFieldSize(name, value)
	if hsize > t.maxSize {
		t.data = t.data[:0]
		t.size = 0
		return false
	}
	t.data = append(t.data, headerField{name, value})
	t.size += hsize
	data := t.data
	for t.size > t.maxSize {
		x := t.data[0]
		t.size -= HeaderFieldSize(x.name, x.value)
		t.data = t.data[1:]
	}
	if len(t.data) != len(data) {
		copy(data, t.data)
		t.data = data[:len(t.data)]
	}
	return true
}

func (t *headerTable) entry(i uint64) (x headerField, ok bool) {
	if ok = i > 0 && i <= uint64(len(staticTable)+len(t.data)); !ok {
		return
	}
	if i <= uint64(len(staticTable)) {
		x = staticTable[i-1]
	} else {
		x = t.data[len(t.data)-(int(i)-len(staticTable))]
	}
	return
}

func (t *headerTable) find(name, value string) (idx uint64, matched bool) {
	if idx, matched = index(name, value); matched {
		return
	}
	i, matched := t.index(name, value)
	if matched || (idx == 0 && i != 0) {
		idx = i + uint64(len(staticTable))
	}
	return
}

func (t *headerTable) index(name, value string) (idx uint64, matched bool) {
	l := len(t.data)
	for i := l - 1; i >= 0; i-- {
		x := t.data[i]
		if matched = strcmp(name, x.name); !matched {
			continue
		}
		if idx == 0 {
			idx = uint64(l - i)
		}
		if matched = strcmp(value, x.value); !matched {
			continue
		}
		idx = uint64(l - i)
		return
	}
	return
}

func index(name, value string) (idx uint64, matched bool) {
	if i, ok := staticMap[name]; ok {
		idx = i
		for i <= uint64(len(staticTable)) {
			x := staticTable[i-1]
			if matched = strcmp(name, x.name); !matched {
				break
			}
			if matched = strcmp(value, x.value); matched {
				idx = i
				break
			}
			i++
		}
	}
	return
}

func strcmp(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	c := byte(0)
	for i := 0; i < len(a); i++ {
		c |= (a[i] ^ b[i])
	}
	return c == 0
}

var (
	staticTable []headerField
	staticMap   map[string]uint64
)

func init() {
	staticTable = []headerField{
		{":authority", ""},
		{":method", "GET"},
		{":method", "POST"},
		{":path", "/"},
		{":path", "/index.html"},
		{":scheme", "http"},
		{":scheme", "https"},
		{":status", "200"},
		{":status", "204"},
		{":status", "206"},
		{":status", "304"},
		{":status", "400"},
		{":status", "404"},
		{":status", "500"},
		{"accept-charset", ""},
		{"accept-encoding", "gzip, deflate"},
		{"accept-language", ""},
		{"accept-ranges", ""},
		{"accept", ""},
		{"access-control-allow-origin", ""},
		{"age", ""},
		{"allow", ""},
		{"authorization", ""},
		{"cache-control", ""},
		{"content-disposition", ""},
		{"content-encoding", ""},
		{"content-language", ""},
		{"content-length", ""},
		{"content-location", ""},
		{"content-range", ""},
		{"content-type", ""},
		{"cookie", ""},
		{"date", ""},
		{"etag", ""},
		{"expect", ""},
		{"expires", ""},
		{"from", ""},
		{"host", ""},
		{"if-match", ""},
		{"if-modified-since", ""},
		{"if-none-match", ""},
		{"if-range", ""},
		{"if-unmodified-since", ""},
		{"last-modified", ""},
		{"link", ""},
		{"location", ""},
		{"max-forwards", ""},
		{"proxy-authenticate", ""},
		{"proxy-authorization", ""},
		{"range", ""},
		{"referer", ""},
		{"refresh", ""},
		{"retry-after", ""},
		{"server", ""},
		{"set-cookie", ""},
		{"strict-transport-security", ""},
		{"transfer-encoding", ""},
		{"user-agent", ""},
		{"vary", ""},
		{"via", ""},
		{"www-authenticate", ""},
	}

	staticMap = make(map[string]uint64)

	for i := len(staticTable); i > 0; i-- {
		x := staticTable[i-1]
		staticMap[x.name] = uint64(i)
	}
}
