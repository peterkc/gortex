package contracts

import "testing"

func TestThriftExtractor_ServiceFunctions(t *testing.T) {
	ext := &ThriftExtractor{}
	src := []byte(`namespace go shared.calc
namespace java com.example.calc

struct Work {
  1: i32 num1,
  2: i32 num2,
}

service Calculator extends shared.SharedService {
   void ping(),

   i32 add(1: i32 num1, 2: i32 num2),

   Work calculate(1: i32 logid, 2: Work w) throws (1: InvalidOperation ouch),

   oneway void zip()
}
`)
	out := ext.Extract("calc.thrift", src, nil, nil)
	if len(out) != 4 {
		t.Fatalf("expected 4 contracts, got %d: %+v", len(out), out)
	}

	byID := map[string]Contract{}
	for _, c := range out {
		byID[c.ID] = c
		if c.Type != ContractThrift {
			t.Errorf("%s: Type = %q, want thrift", c.ID, c.Type)
		}
		if c.Role != RoleProvider {
			t.Errorf("%s: Role = %q, want provider", c.ID, c.Role)
		}
	}

	ping, ok := byID["thrift::Calculator::ping"]
	if !ok {
		t.Fatalf("missing thrift::Calculator::ping; got %v", byID)
	}
	if ping.Meta["canonical"] != "shared.calc.Calculator/ping" {
		t.Errorf("ping canonical = %v", ping.Meta["canonical"])
	}
	if ping.Meta["package"] != "shared.calc" {
		t.Errorf("ping package = %v, want the go namespace", ping.Meta["package"])
	}
	if _, hasResp := ping.Meta["response_type"]; hasResp {
		t.Errorf("void function must not carry response_type: %v", ping.Meta["response_type"])
	}

	add, ok := byID["thrift::Calculator::add"]
	if !ok {
		t.Fatalf("missing thrift::Calculator::add")
	}
	if add.Meta["response_type"] != "i32" {
		t.Errorf("add response_type = %v, want i32", add.Meta["response_type"])
	}
	args, _ := add.Meta["args"].([]string)
	if len(args) != 2 || args[0] != "num1:i32" || args[1] != "num2:i32" {
		t.Errorf("add args = %v, want [num1:i32 num2:i32]", args)
	}

	calc, ok := byID["thrift::Calculator::calculate"]
	if !ok {
		t.Fatalf("missing thrift::Calculator::calculate")
	}
	if calc.Meta["response_type"] != "Work" {
		t.Errorf("calculate response_type = %v, want Work", calc.Meta["response_type"])
	}

	zip, ok := byID["thrift::Calculator::zip"]
	if !ok {
		t.Fatalf("missing thrift::Calculator::zip")
	}
	if zip.Meta["oneway"] != true {
		t.Errorf("zip oneway = %v, want true", zip.Meta["oneway"])
	}

	// Struct fields must not be misread as service functions.
	if _, leaked := byID["thrift::Calculator::num1"]; leaked {
		t.Errorf("struct field leaked into service functions")
	}
}

func TestThriftExtractor_MultipleServicesBounded(t *testing.T) {
	ext := &ThriftExtractor{}
	src := []byte(`namespace py users

service UserService {
  User getUser(1: string id)
}

service AdminService {
  void purge(1: string id)
}
`)
	out := ext.Extract("users.thrift", src, nil, nil)
	if len(out) != 2 {
		t.Fatalf("expected 2 contracts, got %d: %+v", len(out), out)
	}
	got := map[string]bool{}
	for _, c := range out {
		got[c.ID] = true
	}
	if !got["thrift::UserService::getUser"] || !got["thrift::AdminService::purge"] {
		t.Errorf("missing expected contracts: %v", got)
	}
	if got["thrift::UserService::purge"] {
		t.Errorf("AdminService function leaked into UserService")
	}
}

func TestThriftExtractor_ContainerTypesAndNamespacePreference(t *testing.T) {
	ext := &ThriftExtractor{}
	src := []byte(`namespace java com.example.inventory
namespace * inventory

service Inventory {
  list<Item> listItems(1: map<string, i32> filters),
  map<i32, string> labels()
}
`)
	out := ext.Extract("inv.thrift", src, nil, nil)
	if len(out) != 2 {
		t.Fatalf("expected 2 contracts, got %d: %+v", len(out), out)
	}
	byID := map[string]Contract{}
	for _, c := range out {
		byID[c.ID] = c
	}
	li, ok := byID["thrift::Inventory::listItems"]
	if !ok {
		t.Fatalf("missing listItems; got %v", byID)
	}
	// "*" namespace applies to every generator, so it wins over java.
	if li.Meta["package"] != "inventory" {
		t.Errorf("package = %v, want inventory (the * namespace)", li.Meta["package"])
	}
	if li.Meta["response_type"] != "list<Item>" {
		t.Errorf("listItems response_type = %v", li.Meta["response_type"])
	}
	if _, ok := byID["thrift::Inventory::labels"]; !ok {
		t.Errorf("generic return type with no args not extracted")
	}
}

func TestThriftExtractor_NonThriftFileIgnored(t *testing.T) {
	ext := &ThriftExtractor{}
	out := ext.Extract("main.go", []byte("service Foo { void bar() }"), nil, nil)
	if len(out) != 0 {
		t.Fatalf("non-.thrift file must produce no contracts, got %+v", out)
	}
}

func TestThriftExtractor_NamespaceFallback(t *testing.T) {
	ext := &ThriftExtractor{}
	// PHP comes first in the source and its namespace sorts first by value.
	// The fallback must sort by scope instead.
	src := []byte(`namespace php alpha.php
namespace csharp zeta.csharp

service Example {
  void ping()
}
`)
	for i := range 64 {
		out := ext.Extract("example.thrift", src, nil, nil)
		if len(out) != 1 {
			t.Fatalf("run %d: got %d contracts, want 1", i, len(out))
		}
		if got := out[0].Meta["package"]; got != "zeta.csharp" {
			t.Fatalf("run %d: package = %v, want zeta.csharp", i, got)
		}
		if got := out[0].Meta["canonical"]; got != "zeta.csharp.Example/ping" {
			t.Fatalf("run %d: canonical = %v, want zeta.csharp.Example/ping", i, got)
		}
	}
}

func TestPickThriftNamespace_PreferenceOrder(t *testing.T) {
	// Keep the expected order independent of the production list.
	order := []string{"*", "go", "java", "py", "python", "js", "rb", "cpp"}
	namespaces := map[string]string{"aaa": "fallback"}
	for _, scope := range order {
		namespaces[scope] = "preferred:" + scope
	}
	for _, scope := range order {
		if got, want := pickThriftNamespace(namespaces), namespaces[scope]; got != want {
			t.Fatalf("when %q has highest preference, got %q, want %q", scope, got, want)
		}
		delete(namespaces, scope)
	}
}
