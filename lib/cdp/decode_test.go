package cdp

import (
	"testing"
)

func TestScanIncoming(t *testing.T) {
	msg, err := scanIncoming([]byte(`{"method":"Network.requestWillBeSent","params":{"url":"http://a}b","n":1},"sessionId":"abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Method != "Network.requestWillBeSent" || msg.SessionID != "abc" || msg.ID != 0 {
		t.Fatalf("%+v", msg)
	}
	if string(msg.Params) != `{"url":"http://a}b","n":1}` {
		t.Fatalf("params %s", msg.Params)
	}

	msg, err = scanIncoming([]byte(`{"id":7,"result":{"product":"Chrome"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID != 7 || string(msg.Result) != `{"product":"Chrome"}` {
		t.Fatalf("%+v %s", msg, msg.Result)
	}

	msg, err = scanIncoming([]byte(`{"id":8,"error":{"code":-32000,"message":"gone","data":""}}`))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Error == nil || msg.Error.Code != -32000 || msg.Error.Message != "gone" {
		t.Fatalf("%+v", msg.Error)
	}

	msg, err = scanIncoming([]byte("{\n  \"method\" : \"Page.loadEventFired\" , \"params\" : {} }"))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Method != "Page.loadEventFired" || string(msg.Params) != "{}" {
		t.Fatalf("%+v %s", msg, msg.Params)
	}
}

func TestScanIncomingEscapes(t *testing.T) {
	msg, err := scanIncoming([]byte(`{"method":"A\"B","params":{"s":"x\"y"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Method != `A"B` {
		t.Fatalf("method %q", msg.Method)
	}
	if string(msg.Params) != `{"s":"x\"y"}` {
		t.Fatalf("params %s", msg.Params)
	}
}
