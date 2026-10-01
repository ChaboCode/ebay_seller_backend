package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// fakeOdoo simula /jsonrpc: responde según "modelo.método" y registra las
// llamadas recibidas.
type fakeOdoo struct {
	t       *testing.T
	handler func(model, method string, args []any, kwargs map[string]any) any
	calls   []string
	creates map[string][]map[string]any
}

func newFakeOdoo(t *testing.T, handler func(model, method string, args []any, kwargs map[string]any) any) (*fakeOdoo, *OdooClient) {
	f := &fakeOdoo{t: t, handler: handler, creates: map[string][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, NewOdooClient(srv.URL, "db", "user", "key")
}

func (f *fakeOdoo) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Params struct {
			Service string `json:"service"`
			Method  string `json:"method"`
			Args    []any  `json:"args"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Fatal(err)
	}

	var result any
	switch {
	case req.Params.Service == "common" && req.Params.Method == "login":
		result = 2
	case req.Params.Service == "object" && req.Params.Method == "execute_kw":
		a := req.Params.Args
		model, method := a[3].(string), a[4].(string)
		args, _ := a[5].([]any)
		kwargs, _ := a[6].(map[string]any)
		f.calls = append(f.calls, model+"."+method)
		if method == "create" {
			f.creates[model] = append(f.creates[model], args[0].(map[string]any))
		}
		result = f.handler(model, method, args, kwargs)
	default:
		f.t.Fatalf("unexpected call %s.%s", req.Params.Service, req.Params.Method)
	}
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": result})
}

// imageTransport devuelve un PNG para cualquier URL.
type imageTransport struct{ requested []string }

func (it *imageTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	it.requested = append(it.requested, r.URL.String())
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"image/png"}},
		Body:       io.NopCloser(bytes.NewReader([]byte("png-bytes"))),
		Request:    r,
	}, nil
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike(`sakura_04%18\`); got != `sakura\_04\%18\\` {
		t.Errorf("escapeLike = %q", got)
	}
}

func TestMany2oneUnmarshal(t *testing.T) {
	var v struct {
		A many2one `json:"a"`
		B many2one `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"a":[7,"P00007"],"b":false}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A.ID != 7 || v.A.Name != "P00007" || v.B.ID != 0 {
		t.Errorf("got %+v", v)
	}
}

func TestOdooErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"jsonrpc":"2.0","error":{"code":200,"message":"Odoo Server Error","data":{"name":"odoo.exceptions.AccessError","message":"You are not allowed"}}}`)
	}))
	defer srv.Close()
	odoo := NewOdooClient(srv.URL, "db", "user", "key")
	odoo.uid = 2

	err := odoo.Write("res.partner", []int{1}, map[string]any{"name": "x"})
	if err == nil || err.Error() != "odoo: You are not allowed" {
		t.Errorf("err = %v", err)
	}
}

// Alta nueva (Odoo 18): vendedor inexistente, producto inexistente.
func TestUpsertCreatesVendorProductAndOrder(t *testing.T) {
	orderCreated := false
	f, odoo := newFakeOdoo(t, func(model, method string, args []any, kwargs map[string]any) any {
		switch model + "." + method {
		case "product.product.fields_get":
			return map[string]any{"uom_po_id": map[string]any{"type": "many2one"}}
		case "product.template.fields_get":
			return map[string]any{"is_storable": map[string]any{"type": "boolean"}}
		case "purchase.order.line.fields_get":
			return map[string]any{"product_uom": map[string]any{"type": "many2one"}}
		case "product.product.search_read":
			domain := args[0].([]any)[0].([]any)
			if domain[0] == "product_tmpl_id" {
				return []any{map[string]any{"id": 11, "default_code": "EBAY-123", "uom_id": []any{1, "Units"}, "uom_po_id": []any{1, "Units"}}}
			}
			if !orderCreated {
				return []any{}
			}
			return []any{map[string]any{"id": 11, "default_code": "EBAY-123"}}
		case "purchase.order.line.search_read":
			if !orderCreated {
				return []any{}
			}
			return []any{map[string]any{
				"id": 5, "product_id": []any{11, "x"}, "order_id": []any{9, "P00009 (123)"},
				"price_unit": 45.5, "qty_received": 0, "state": "draft",
				"currency_id": []any{1, "USD"}, "partner_id": []any{20, "@sakura0418"},
			}}
		case "purchase.order.search_read":
			return []any{map[string]any{"id": 9, "name": "P00009"}}
		case "res.partner.search_read":
			return []any{}
		case "res.partner.create":
			return 20
		case "product.template.create":
			return 30
		case "res.currency.search_read":
			return []any{map[string]any{"id": 1}}
		case "purchase.order.create":
			orderCreated = true
			return 9
		}
		t.Fatalf("unexpected %s.%s", model, method)
		return nil
	})

	images := &imageTransport{}
	bridge := NewOdooBridge(odoo, &http.Client{Transport: images})

	cost, bid := 45.5, 42.0
	st, created, err := bridge.Upsert("123", odooPurchaseRequest{
		Title:        "Figura Sakura",
		ImageURL:     "https://i.ebayimg.com/images/g/abc/s-l225.jpg",
		Seller:       "@sakura0418",
		Cost:         &cost,
		Currency:     "usd",
		AuctionPrice: &bid,
		ListingURL:   "https://www.ebay.com/itm/123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || !st.InOdoo || st.OrderName != "P00009" || st.State != "draft" || *st.Cost != 45.5 {
		t.Errorf("status = %+v", st)
	}

	partner := f.creates["res.partner"][0]
	if partner["name"] != "@sakura0418" || partner["ref"] != "sakura0418" || partner["supplier_rank"] != float64(1) {
		t.Errorf("partner = %v", partner)
	}

	tmpl := f.creates["product.template"][0]
	if tmpl["default_code"] != "EBAY-123" || tmpl["type"] != "consu" || tmpl["is_storable"] != true {
		t.Errorf("template = %v", tmpl)
	}
	if tmpl["image_1920"] != "cG5nLWJ5dGVz" { // base64("png-bytes")
		t.Errorf("image_1920 = %v", tmpl["image_1920"])
	}
	if len(images.requested) != 1 || !strings.HasSuffix(images.requested[0], "/s-l1600.jpg") {
		t.Errorf("image requests = %v", images.requested)
	}

	order := f.creates["purchase.order"][0]
	if order["partner_id"] != float64(20) || order["partner_ref"] != "123" || order["currency_id"] != float64(1) {
		t.Errorf("order = %v", order)
	}
	line := order["order_line"].([]any)[0].([]any)[2].(map[string]any)
	if line["product_id"] != float64(11) || line["price_unit"] != 45.5 || line["product_uom"] != float64(1) {
		t.Errorf("line = %v", line)
	}
	if !strings.Contains(line["name"].(string), "42.00 USD") {
		t.Errorf("line name = %q", line["name"])
	}
}

// Ya existe una orden: solo se actualiza el costo; si está bloqueada, 409.
func TestUpsertUpdatesCostOrRejectsLocked(t *testing.T) {
	state := "purchase"
	f, odoo := newFakeOdoo(t, func(model, method string, args []any, kwargs map[string]any) any {
		switch model + "." + method {
		case "product.product.search_read":
			return []any{map[string]any{"id": 11, "default_code": "EBAY-123"}}
		case "purchase.order.line.search_read":
			return []any{map[string]any{
				"id": 5, "product_id": []any{11, "x"}, "order_id": []any{9, "P00009"},
				"price_unit": 10, "state": state, "currency_id": []any{1, "USD"},
			}}
		case "purchase.order.search_read":
			return []any{map[string]any{"id": 9, "name": "P00009"}}
		case "purchase.order.line.write", "product.product.write":
			if args[1].(map[string]any)["price_unit"] != nil && args[1].(map[string]any)["price_unit"] != 12.35 {
				t.Errorf("write %v", args)
			}
			return true
		}
		t.Fatalf("unexpected %s.%s", model, method)
		return nil
	})
	bridge := NewOdooBridge(odoo, http.DefaultClient)

	cost := 12.345
	_, created, err := bridge.Upsert("123", odooPurchaseRequest{Cost: &cost})
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if !strings.Contains(strings.Join(f.calls, ","), "purchase.order.line.write") {
		t.Errorf("calls = %v", f.calls)
	}

	state = "done"
	_, _, err = bridge.Upsert("123", odooPurchaseRequest{Cost: &cost})
	if _, ok := err.(errConflict); !ok {
		t.Errorf("err = %v, want conflict", err)
	}
}

func TestOdooRoutesAuthAndDisabled(t *testing.T) {
	t.Setenv("ODOO_BRIDGE_TOKEN", "secret")
	router := gin.New()
	RegisterOdooRoutes(router, nil)

	for _, tc := range []struct {
		key  string
		want int
	}{
		{"", http.StatusUnauthorized},
		{"wrong", http.StatusUnauthorized},
		{"secret", http.StatusServiceUnavailable},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/odoo/status", nil)
		if tc.key != "" {
			req.Header.Set("X-Api-Key", tc.key)
		}
		router.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("key %q: status %d, want %d", tc.key, w.Code, tc.want)
		}
	}
}
