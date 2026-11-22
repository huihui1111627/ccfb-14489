// Package acceptance benchmarks the transcode pipeline. The numbers feed the
// throughput/latency summary in README.md.
package acceptance

import (
	"context"
	"path/filepath"
	"testing"

	"example.com/idlbridge/internal/backend"
	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/gateway"
	"example.com/idlbridge/internal/idl"
	"example.com/idlbridge/internal/jsoncodec"
	"example.com/idlbridge/internal/routing"
	"example.com/idlbridge/internal/rpc"
	"example.com/idlbridge/internal/transcode"
	"example.com/idlbridge/internal/wire"
)

func benchSchema(b *testing.B) *gateway.Schema {
	b.Helper()
	s, err := gateway.LoadDir(filepath.Join("..", "testdata"))
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// BenchmarkRouteMatch measures pure path routing.
func BenchmarkRouteMatch(b *testing.B) {
	s := benchSchema(b)
	path := "/v1/shelves/fiction/books/dune"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := s.Table.Lookup("GET", path); !ok {
			b.Fatal("no match")
		}
	}
}

// BenchmarkBindQuery measures query/path binding into a request message.
func BenchmarkBindQuery(b *testing.B) {
	s := benchSchema(b)
	match, ok := s.Table.Lookup("GET", "/v1/shelves/fiction/books")
	if !ok {
		b.Fatal("no match")
	}
	bind := match.Route.Data.(*transcode.Binding)
	query, err := routing.ParseQuery("page_size=2&genre=FICTION&tags=a&tags=b&include_archived=true")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := transcode.BindRequest(bind, match, nil, query, transcode.BindOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBindBody measures JSON body binding into a request message.
func BenchmarkBindBody(b *testing.B) {
	s := benchSchema(b)
	match, _ := s.Table.Lookup("POST", "/v1/shelves/fiction/books")
	bind := match.Route.Data.(*transcode.Binding)
	body := []byte(`{"name":"dune","title":"Dune","isbn":"9780441172719","genre":"FICTION","authors":["Herbert"]}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := transcode.BindRequest(bind, match, body, nil, transcode.BindOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeDecode measures the wire codec on a realistic message.
func BenchmarkEncodeDecode(b *testing.B) {
	s := benchSchema(b)
	desc := s.Registry.Message("library.v1.ScalarBag")
	m := sampleBag(desc)
	raw, err := wire.Marshal(m)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.Run("encode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := wire.Marshal(m); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("decode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := wire.Unmarshal(raw, desc); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkJSONRoundTrip measures the JSON mapping both ways.
func BenchmarkJSONRoundTrip(b *testing.B) {
	s := benchSchema(b)
	desc := s.Registry.Message("library.v1.ScalarBag")
	m := sampleBag(desc)
	doc, err := jsoncodec.Marshal(m, jsoncodec.Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.Run("marshal", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := jsoncodec.Marshal(m, jsoncodec.Options{}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("unmarshal", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := jsoncodec.Unmarshal(doc, desc, jsoncodec.Options{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkEndToEnd measures a full HTTP-shaped transcode plus backend call.
func BenchmarkEndToEnd(b *testing.B) {
	s := benchSchema(b)
	fake := backend.New(s.Registry)
	router := fake.Router()
	h, err := gateway.New(gateway.Options{Schema: s, Backend: router})
	if err != nil {
		b.Fatal(err)
	}
	body := []byte(`{"title":"Dune","isbn":"9780441172719","genre":"FICTION","authors":["Herbert"]}`)
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		res, err := h.Plan(gateway.TranscodeRequest{Method: "POST", Path: "/v1/shelves/fiction/books", Body: body})
		if err != nil || res.Request == nil {
			b.Fatal(err)
		}
		if _, err := router.Unary(ctx, &rpc.Request{Method: res.Binding.Method, Message: res.Request}); err != nil {
			b.Fatal(err)
		}
	}
}

func sampleBag(desc *idl.Message) *dyn.Message {
	m := dyn.New(desc)
	m.Fields[1] = 1.5
	m.Fields[2] = float32(-2.25)
	m.Fields[3] = int32(-1)
	m.Fields[4] = int64(-2)
	m.Fields[5] = uint32(300)
	m.Fields[6] = uint64(1 << 40)
	m.Fields[7] = int32(-7)
	m.Fields[8] = int64(-8)
	m.Fields[9] = uint32(0xDEADBEEF)
	m.Fields[10] = uint64(0x0102030405060708)
	m.Fields[11] = int32(-9)
	m.Fields[12] = int64(-10)
	m.Fields[13] = true
	m.Fields[14] = "hello, world"
	m.Fields[15] = []byte{0, 1, 2, 3, 0xff}
	m.Fields[16] = int32(2)
	m.Fields[17] = []any{int32(1), int32(-1), int32(2), int32(1000)}
	m.Fields[18] = []any{int32(5), int32(6)}
	shelf := dyn.New(desc.FieldByNumber(20).MapValue.Message)
	shelf.Fields[1] = "sci"
	shelf.Fields[2] = "science"
	m.Fields[20] = dyn.Map{"top": shelf}
	return m
}
