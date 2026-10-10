package hashing

import "testing"

func TestCanonicalIsKeyOrderIndependent(t *testing.T) {
	a, err := Content([]byte(`{"b":1,"a":{"y":[1,2],"x":"é<>"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Content([]byte(" { \"a\" : {\"x\":\"é<>\",\"y\":[1,2]}, \"b\":1 }\n"))
	if a != b {
		t.Fatal("canonical hash must not depend on key order or whitespace")
	}
	c, _ := Content([]byte(`{"b":2,"a":{"y":[1,2],"x":"é<>"}}`))
	if a == c {
		t.Fatal("different content must hash differently")
	}
	if _, err := Content([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Fatal("trailing data must be rejected")
	}
}

func TestRefAndExtRef(t *testing.T) {
	if Ref([]byte("s1"), DomainAccount, "x") == Ref([]byte("s2"), DomainAccount, "x") {
		t.Fatal("salt must matter")
	}
	if Ref([]byte("s"), DomainAccount, "x") == Ref([]byte("s"), DomainStaff, "x") {
		t.Fatal("domain must matter")
	}
	h := ExtRef("WB20261004")
	if string(h[:10]) != "WB20261004" || h[10] != 0 {
		t.Fatal("short refs stay readable")
	}
	if ExtRef("a-very-long-external-reference-over-32-bytes") != Text("a-very-long-external-reference-over-32-bytes") {
		t.Fatal("long refs are hashed")
	}
	if !ExtRef("").IsZero() {
		t.Fatal("empty ref is zero")
	}
}
