package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OdooClient habla con Odoo por JSON-RPC (/jsonrpc), la API externa que
// existe en todas las versiones soportadas (16, 17, 18 y 19).
//
// Se configura con variables de entorno:
//
//	ODOO_URL      https://mi-empresa.odoo.com
//	ODOO_DB       nombre de la base de datos
//	ODOO_USERNAME login del usuario (ej. admin@empresa.com)
//	ODOO_API_KEY  API key del usuario (Preferencias → Seguridad de la cuenta)
//	              o su contraseña
type OdooClient struct {
	baseURL    string
	db         string
	username   string
	password   string
	httpClient *http.Client

	mu  sync.Mutex
	uid int

	reqID atomic.Int64

	fieldsMu sync.Mutex
	fields   map[string]map[string]odooField
}

// odooField es el subconjunto de fields_get que se usa para adaptarse a la
// versión de Odoo (los nombres de campos cambian entre versiones).
type odooField struct {
	Type      string  `json:"type"`
	Selection [][]any `json:"selection"`
}

// NewOdooClientFromEnv devuelve nil si Odoo no está configurado: los
// endpoints /odoo/* responden 503 en ese caso.
func NewOdooClientFromEnv() *OdooClient {
	baseURL := strings.TrimRight(os.Getenv("ODOO_URL"), "/")
	db := os.Getenv("ODOO_DB")
	username := os.Getenv("ODOO_USERNAME")
	password := os.Getenv("ODOO_API_KEY")
	if password == "" {
		password = os.Getenv("ODOO_PASSWORD")
	}
	if baseURL == "" || db == "" || username == "" || password == "" {
		return nil
	}
	return NewOdooClient(baseURL, db, username, password)
}

func NewOdooClient(baseURL, db, username, password string) *OdooClient {
	return &OdooClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		db:         db,
		username:   username,
		password:   password,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		fields:     make(map[string]map[string]odooField),
	}
}

// OdooError es un error devuelto por el servidor de Odoo (validación,
// permisos, etc.). Data.Message trae el mensaje legible para el usuario.
type OdooError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"data"`
}

func (e *OdooError) Error() string {
	if e.Data.Message != "" {
		return "odoo: " + e.Data.Message
	}
	return "odoo: " + e.Message
}

var errOdooAuth = errors.New("odoo: login failed, check ODOO_DB / ODOO_USERNAME / ODOO_API_KEY")

// call hace una llamada JSON-RPC a /jsonrpc y decodifica `result` en out.
func (c *OdooClient) call(service, method string, args []any, out any) error {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "call",
		"id":      c.reqID.Add(1),
		"params": map[string]any{
			"service": service,
			"method":  method,
			"args":    args,
		},
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/jsonrpc", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("odoo: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("odoo: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("odoo: %s: %s", resp.Status, truncate(string(body), 300))
	}

	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *OdooError      `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return fmt.Errorf("odoo: decoding response: %w", err)
	}
	if rpc.Error != nil {
		return rpc.Error
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rpc.Result, out)
}

// login autentica una vez y guarda el uid.
func (c *OdooClient) login() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.uid != 0 {
		return c.uid, nil
	}

	// login devuelve el uid, o false si las credenciales no son válidas.
	var raw json.RawMessage
	if err := c.call("common", "login", []any{c.db, c.username, c.password}, &raw); err != nil {
		return 0, err
	}
	var uid int
	if err := json.Unmarshal(raw, &uid); err != nil || uid == 0 {
		return 0, errOdooAuth
	}
	c.uid = uid
	return uid, nil
}

// Version devuelve la info de common.version (no requiere login).
func (c *OdooClient) Version() (map[string]any, error) {
	var v map[string]any
	err := c.call("common", "version", []any{}, &v)
	return v, err
}

// ExecuteKw llama a un método de un modelo: model.method(*args, **kwargs).
func (c *OdooClient) ExecuteKw(model, method string, args []any, kwargs map[string]any, out any) error {
	uid, err := c.login()
	if err != nil {
		return err
	}
	if args == nil {
		args = []any{}
	}
	if kwargs == nil {
		kwargs = map[string]any{}
	}
	return c.call("object", "execute_kw",
		[]any{c.db, uid, c.password, model, method, args, kwargs}, out)
}

// SearchRead es model.search_read(domain, fields=..., ...).
func (c *OdooClient) SearchRead(model string, domain []any, fields []string, kwargs map[string]any, out any) error {
	kw := map[string]any{"fields": fields}
	for k, v := range kwargs {
		kw[k] = v
	}
	if domain == nil {
		domain = []any{}
	}
	return c.ExecuteKw(model, "search_read", []any{domain}, kw, out)
}

// Create crea un registro y devuelve su id.
func (c *OdooClient) Create(model string, vals map[string]any) (int, error) {
	var id int
	err := c.ExecuteKw(model, "create", []any{vals}, nil, &id)
	return id, err
}

// Write actualiza los registros ids con vals.
func (c *OdooClient) Write(model string, ids []int, vals map[string]any) error {
	return c.ExecuteKw(model, "write", []any{ids, vals}, nil, nil)
}

// Fields devuelve (y cachea) la definición de los campos de un modelo.
func (c *OdooClient) Fields(model string) (map[string]odooField, error) {
	c.fieldsMu.Lock()
	defer c.fieldsMu.Unlock()

	if f, ok := c.fields[model]; ok {
		return f, nil
	}
	var f map[string]odooField
	err := c.ExecuteKw(model, "fields_get", nil,
		map[string]any{"attributes": []string{"type", "selection"}}, &f)
	if err != nil {
		return nil, err
	}
	c.fields[model] = f
	return f, nil
}

// HasField indica si model tiene el campo name en esta versión de Odoo.
func (c *OdooClient) HasField(model, name string) (bool, error) {
	f, err := c.Fields(model)
	if err != nil {
		return false, err
	}
	_, ok := f[name]
	return ok, nil
}

// RecordURL es el enlace al formulario de un registro en la interfaz web.
// /web#... funciona en todas las versiones (en 17+ redirige a /odoo/...).
func (c *OdooClient) RecordURL(model string, id int) string {
	return fmt.Sprintf("%s/web#id=%d&model=%s&view_type=form", c.baseURL, id, model)
}

// many2one representa un valor Many2one tal como lo devuelve search_read:
// [id, "nombre"] o false.
type many2one struct {
	ID   int
	Name string
}

func (m *many2one) UnmarshalJSON(b []byte) error {
	if string(b) == "false" || string(b) == "null" {
		*m = many2one{}
		return nil
	}
	var pair []any
	if err := json.Unmarshal(b, &pair); err != nil {
		return err
	}
	if len(pair) > 0 {
		if id, ok := pair[0].(float64); ok {
			m.ID = int(id)
		}
	}
	if len(pair) > 1 {
		if name, ok := pair[1].(string); ok {
			m.Name = name
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
