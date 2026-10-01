package main

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// OdooBridge registra figuras de eBay en el módulo de Compras de Odoo.
//
// Cada figura es un producto con referencia interna EBAY-<legacyItemId>, y
// se compra con una orden de compra (RFQ en borrador) al vendedor de eBay,
// que se da de alta como contacto si no existe. Después, en Odoo, se
// confirma la orden para que la recepción entre al inventario.
type OdooBridge struct {
	odoo   *OdooClient
	images *http.Client

	// writeMu serializa las altas, para que dos toques seguidos sobre la
	// misma figura no creen dos órdenes de compra.
	writeMu sync.Mutex
}

func NewOdooBridge(odoo *OdooClient, images *http.Client) *OdooBridge {
	return &OdooBridge{odoo: odoo, images: images}
}

const (
	odooProductCodePrefix = "EBAY-"
	maxLookupItems        = 1000
)

var (
	legacyItemIDRe = regexp.MustCompile(`^[0-9]{1,20}$`)
	sellerNameRe   = regexp.MustCompile(`^[^\s]{1,64}$`)
)

func odooProductCode(legacyItemID string) string {
	return odooProductCodePrefix + legacyItemID
}

// odooPurchaseStatus es el estado de una figura en Compras de Odoo.
type odooPurchaseStatus struct {
	ItemID      string   `json:"itemId"`
	InOdoo      bool     `json:"inOdoo"`
	ProductID   int      `json:"productId,omitempty"`
	OrderID     int      `json:"orderId,omitempty"`
	OrderName   string   `json:"orderName,omitempty"`
	State       string   `json:"state,omitempty"` // draft, sent, to approve, purchase, done
	Cost        *float64 `json:"cost,omitempty"`  // price_unit de la línea
	Currency    string   `json:"currency,omitempty"`
	QtyReceived float64  `json:"qtyReceived"`
	Vendor      string   `json:"vendor,omitempty"`
	Editable    bool     `json:"editable"`
	OdooURL     string   `json:"odooUrl,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

// odooPurchaseRequest es el cuerpo de PUT /odoo/purchases/:itemId.
type odooPurchaseRequest struct {
	Title        string   `json:"title"`
	ImageURL     string   `json:"imageUrl"`
	Seller       string   `json:"seller"`
	Cost         *float64 `json:"cost"`
	Currency     string   `json:"currency"`
	AuctionPrice *float64 `json:"auctionPrice"`
	ListingURL   string   `json:"listingUrl"`
}

type odooLineRow struct {
	ID          int      `json:"id"`
	ProductID   many2one `json:"product_id"`
	OrderID     many2one `json:"order_id"`
	PriceUnit   float64  `json:"price_unit"`
	QtyReceived float64  `json:"qty_received"`
	State       string   `json:"state"`
	CurrencyID  many2one `json:"currency_id"`
	PartnerID   many2one `json:"partner_id"`
}

type odooProductRow struct {
	ID          int      `json:"id"`
	DefaultCode string   `json:"default_code"`
	UomID       many2one `json:"uom_id"`
	UomPoID     many2one `json:"uom_po_id"`
}

// Lookup devuelve el estado en Odoo de varias figuras (por legacyItemId).
// Las que no están en Odoo vienen con InOdoo=false.
func (b *OdooBridge) Lookup(itemIDs []string) (map[string]*odooPurchaseStatus, error) {
	result := make(map[string]*odooPurchaseStatus, len(itemIDs))
	codes := make([]string, 0, len(itemIDs))
	for _, id := range itemIDs {
		if _, dup := result[id]; dup {
			continue
		}
		result[id] = &odooPurchaseStatus{ItemID: id}
		codes = append(codes, odooProductCode(id))
	}
	if len(codes) == 0 {
		return result, nil
	}

	var products []odooProductRow
	err := b.odoo.SearchRead("product.product",
		[]any{[]any{"default_code", "in", codes}},
		[]string{"id", "default_code"},
		map[string]any{"context": map[string]any{"active_test": false}},
		&products)
	if err != nil {
		return nil, err
	}
	if len(products) == 0 {
		return result, nil
	}

	itemByProduct := make(map[int]string, len(products))
	productIDs := make([]int, 0, len(products))
	for _, p := range products {
		id := strings.TrimPrefix(p.DefaultCode, odooProductCodePrefix)
		if st, ok := result[id]; ok {
			st.ProductID = p.ID
			itemByProduct[p.ID] = id
			productIDs = append(productIDs, p.ID)
		}
	}

	// La línea más reciente no cancelada de cada producto.
	var lines []odooLineRow
	err = b.odoo.SearchRead("purchase.order.line",
		[]any{
			[]any{"product_id", "in", productIDs},
			[]any{"state", "!=", "cancel"},
		},
		[]string{"id", "product_id", "order_id", "price_unit", "qty_received", "state", "currency_id", "partner_id"},
		map[string]any{"order": "id desc"},
		&lines)
	if err != nil {
		return nil, err
	}

	orderIDs := make([]int, 0, len(lines))
	for _, l := range lines {
		st := result[itemByProduct[l.ProductID.ID]]
		if st == nil || st.InOdoo {
			continue
		}
		cost := l.PriceUnit
		st.InOdoo = true
		st.OrderID = l.OrderID.ID
		st.OrderName = l.OrderID.Name
		st.State = l.State
		st.Cost = &cost
		st.Currency = l.CurrencyID.Name
		st.QtyReceived = l.QtyReceived
		st.Vendor = l.PartnerID.Name
		st.Editable = l.State != "done"
		st.OdooURL = b.odoo.RecordURL("purchase.order", l.OrderID.ID)
		orderIDs = append(orderIDs, l.OrderID.ID)
	}

	// El display_name de la orden puede incluir la referencia del
	// proveedor ("P00012 (1234)"); se lee el nombre limpio.
	if len(orderIDs) > 0 {
		var orders []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}
		err = b.odoo.SearchRead("purchase.order",
			[]any{[]any{"id", "in", orderIDs}}, []string{"id", "name"}, nil, &orders)
		if err != nil {
			return nil, err
		}
		names := make(map[int]string, len(orders))
		for _, o := range orders {
			names[o.ID] = o.Name
		}
		for _, st := range result {
			if name, ok := names[st.OrderID]; ok {
				st.OrderName = name
			}
		}
	}

	return result, nil
}

func (b *OdooBridge) status(itemID string) (*odooPurchaseStatus, error) {
	m, err := b.Lookup([]string{itemID})
	if err != nil {
		return nil, err
	}
	return m[itemID], nil
}

// errValidation marca errores causados por la petición (respuesta 400).
type errValidation struct{ msg string }

func (e errValidation) Error() string { return e.msg }

// errConflict marca una figura cuya compra ya no se puede modificar (409).
type errConflict struct{ msg string }

func (e errConflict) Error() string { return e.msg }

// Upsert registra la figura en Compras, o actualiza su costo si ya hay una
// orden de compra (no cancelada) para ella. Devuelve el estado resultante y
// si se creó una orden nueva.
func (b *OdooBridge) Upsert(itemID string, req odooPurchaseRequest) (*odooPurchaseStatus, bool, error) {
	req.Title = strings.TrimSpace(req.Title)
	req.Seller = strings.TrimPrefix(strings.TrimSpace(req.Seller), "@")
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))

	if req.Cost == nil || *req.Cost < 0 || math.IsNaN(*req.Cost) || math.IsInf(*req.Cost, 0) {
		return nil, false, errValidation{"cost must be a number >= 0"}
	}
	cost := math.Round(*req.Cost*100) / 100

	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	current, err := b.status(itemID)
	if err != nil {
		return nil, false, err
	}

	// Ya está en Compras: solo se actualiza el costo.
	if current.InOdoo {
		if !current.Editable {
			return nil, false, errConflict{fmt.Sprintf("purchase order %s is locked in Odoo", current.OrderName)}
		}
		if err := b.updateCost(current, cost); err != nil {
			return nil, false, err
		}
		st, err := b.status(itemID)
		return st, false, err
	}

	if req.Title == "" {
		return nil, false, errValidation{"title is required"}
	}
	if !sellerNameRe.MatchString(req.Seller) {
		return nil, false, errValidation{"seller must be an eBay username, e.g. sakura0418"}
	}

	var warnings []string

	vendorID, err := b.findOrCreateVendor(req.Seller)
	if err != nil {
		return nil, false, err
	}

	product, err := b.findProduct(itemID)
	if err != nil {
		return nil, false, err
	}
	if product == nil {
		image, err := b.fetchImageBase64(req.ImageURL)
		if err != nil {
			log.Printf("odoo: item %s: image not attached: %v", itemID, err)
			warnings = append(warnings, "image could not be attached")
		}
		product, err = b.createProduct(itemID, req, cost, image)
		if err != nil {
			return nil, false, err
		}
	}

	currencyID := 0
	if req.Currency != "" {
		currencyID, err = b.findCurrency(req.Currency)
		if err != nil {
			return nil, false, err
		}
		if currencyID == 0 {
			warnings = append(warnings, fmt.Sprintf(
				"currency %s is not active in Odoo; the order uses the default currency", req.Currency))
		}
	}

	if err := b.createPurchaseOrder(itemID, req, cost, vendorID, currencyID, product); err != nil {
		return nil, false, err
	}

	st, err := b.status(itemID)
	if err != nil {
		return nil, false, err
	}
	st.Warnings = warnings
	return st, true, nil
}

func (b *OdooBridge) updateCost(st *odooPurchaseStatus, cost float64) error {
	var lines []struct {
		ID int `json:"id"`
	}
	err := b.odoo.SearchRead("purchase.order.line",
		[]any{
			[]any{"order_id", "=", st.OrderID},
			[]any{"product_id", "=", st.ProductID},
		},
		[]string{"id"}, nil, &lines)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return fmt.Errorf("odoo: purchase line for %s not found", st.ItemID)
	}
	ids := make([]int, len(lines))
	for i, l := range lines {
		ids[i] = l.ID
	}
	if err := b.odoo.Write("purchase.order.line", ids, map[string]any{"price_unit": cost}); err != nil {
		return err
	}
	return b.odoo.Write("product.product", []int{st.ProductID}, map[string]any{"standard_price": cost})
}

// escapeLike escapa los comodines de LIKE, para que =ilike compare un
// nombre de usuario literal ("_" es común en usernames de eBay).
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// findOrCreateVendor busca al vendedor de eBay por su username (en la
// referencia del contacto, o como nombre "sakura0418" / "@sakura0418"), sin
// distinguir mayúsculas. Si no existe, lo crea como proveedor "@username".
func (b *OdooBridge) findOrCreateVendor(username string) (int, error) {
	u := escapeLike(username)
	var partners []struct {
		ID int `json:"id"`
	}
	err := b.odoo.SearchRead("res.partner",
		[]any{
			"|", "|",
			[]any{"ref", "=ilike", u},
			[]any{"name", "=ilike", u},
			[]any{"name", "=ilike", "@" + u},
		},
		[]string{"id"},
		map[string]any{"limit": 1, "order": "supplier_rank desc, id"},
		&partners)
	if err != nil {
		return 0, err
	}
	if len(partners) > 0 {
		return partners[0].ID, nil
	}

	return b.odoo.Create("res.partner", map[string]any{
		"name":          "@" + username,
		"ref":           username,
		"is_company":    true,
		"supplier_rank": 1,
		"website":       "https://www.ebay.com/usr/" + url.PathEscape(username),
		"comment":       "Vendedor de eBay",
	})
}

func (b *OdooBridge) productFields() []string {
	fields := []string{"id", "default_code", "uom_id"}
	// uom_po_id (unidad de compra) existe hasta Odoo 18.
	if ok, _ := b.odoo.HasField("product.product", "uom_po_id"); ok {
		fields = append(fields, "uom_po_id")
	}
	return fields
}

func (b *OdooBridge) findProduct(itemID string) (*odooProductRow, error) {
	var products []odooProductRow
	err := b.odoo.SearchRead("product.product",
		[]any{[]any{"default_code", "=", odooProductCode(itemID)}},
		b.productFields(),
		map[string]any{"limit": 1, "context": map[string]any{"active_test": false}},
		&products)
	if err != nil || len(products) == 0 {
		return nil, err
	}
	return &products[0], nil
}

func (b *OdooBridge) createProduct(itemID string, req odooPurchaseRequest, cost float64, imageB64 string) (*odooProductRow, error) {
	vals := map[string]any{
		"name":           req.Title,
		"default_code":   odooProductCode(itemID),
		"purchase_ok":    true,
		"sale_ok":        true,
		"standard_price": cost,
	}
	if req.ListingURL != "" {
		vals["description_purchase"] = req.ListingURL
	}
	if imageB64 != "" {
		vals["image_1920"] = imageB64
	}

	// Producto almacenable, para que la recepción entre al inventario.
	// Odoo 18+: type "consu" + is_storable. Odoo ≤17: type "product".
	tmplFields, err := b.odoo.Fields("product.template")
	if err != nil {
		return nil, err
	}
	vals["type"] = "consu"
	if _, ok := tmplFields["is_storable"]; ok {
		vals["is_storable"] = true
	} else if f, ok := tmplFields["type"]; ok && selectionHas(f.Selection, "product") {
		vals["type"] = "product"
	}

	tmplID, err := b.odoo.Create("product.template", vals)
	if err != nil {
		return nil, err
	}

	var products []odooProductRow
	err = b.odoo.SearchRead("product.product",
		[]any{[]any{"product_tmpl_id", "=", tmplID}},
		b.productFields(), map[string]any{"limit": 1}, &products)
	if err != nil {
		return nil, err
	}
	if len(products) == 0 {
		return nil, fmt.Errorf("odoo: product variant for template %d not found", tmplID)
	}
	return &products[0], nil
}

func selectionHas(selection [][]any, key string) bool {
	for _, opt := range selection {
		if len(opt) > 0 && opt[0] == key {
			return true
		}
	}
	return false
}

func (b *OdooBridge) findCurrency(code string) (int, error) {
	var currencies []struct {
		ID int `json:"id"`
	}
	err := b.odoo.SearchRead("res.currency",
		[]any{[]any{"name", "=", code}}, []string{"id"},
		map[string]any{"limit": 1}, &currencies)
	if err != nil || len(currencies) == 0 {
		return 0, err
	}
	return currencies[0].ID, nil
}

func (b *OdooBridge) createPurchaseOrder(itemID string, req odooPurchaseRequest, cost float64, vendorID, currencyID int, product *odooProductRow) error {
	description := req.Title + "\neBay #" + itemID
	if req.ListingURL != "" {
		description += "\n" + req.ListingURL
	}
	if req.AuctionPrice != nil {
		description += fmt.Sprintf("\nPrecio en eBay al registrar: %.2f %s", *req.AuctionPrice, req.Currency)
	}

	line := map[string]any{
		"product_id":  product.ID,
		"name":        description,
		"product_qty": 1,
		"price_unit":  cost,
	}

	// La unidad de medida de la línea se llama product_uom hasta Odoo 18
	// y product_uom_id desde Odoo 19.
	uom := product.UomPoID.ID
	if uom == 0 {
		uom = product.UomID.ID
	}
	if uom != 0 {
		lineFields, err := b.odoo.Fields("purchase.order.line")
		if err != nil {
			return err
		}
		if _, ok := lineFields["product_uom_id"]; ok {
			line["product_uom_id"] = uom
		} else if _, ok := lineFields["product_uom"]; ok {
			line["product_uom"] = uom
		}
	}

	order := map[string]any{
		"partner_id":  vendorID,
		"partner_ref": itemID,
		"origin":      "eBay " + itemID,
		"order_line":  []any{[]any{0, 0, line}},
	}
	if currencyID != 0 {
		order["currency_id"] = currencyID
	}

	_, err := b.odoo.Create("purchase.order", order)
	return err
}

// fetchImageBase64 descarga la imagen (en alta resolución) desde eBay.
func (b *OdooBridge) fetchImageBase64(rawURL string) (string, error) {
	if rawURL == "" {
		return "", errors.New("no image url")
	}
	u, err := url.Parse(largeImageURL(rawURL))
	if err != nil || !isAllowedImageURL(u) {
		return "", errors.New("invalid or disallowed image url")
	}

	resp, err := b.images.Get(u.String())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image fetch failed: %s", resp.Status)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
		return "", errors.New("upstream response is not an image")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxImageBytes {
		return "", errors.New("image too large")
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

// RegisterOdooRoutes monta /odoo/*. Si bridge es nil (Odoo sin configurar)
// todas las rutas responden 503.
//
// Si ODOO_BRIDGE_TOKEN está definido, las rutas exigen el header
// X-Api-Key con ese valor.
func RegisterOdooRoutes(router *gin.Engine, bridge *OdooBridge) {
	group := router.Group("/odoo")
	group.Use(odooAuthMiddleware(os.Getenv("ODOO_BRIDGE_TOKEN")))
	group.Use(func(ctx *gin.Context) {
		if bridge == nil {
			ctx.AbortWithStatusJSON(http.StatusServiceUnavailable,
				gin.H{"error": "Odoo is not configured on the server"})
			return
		}
		ctx.Next()
	})

	group.GET("/status", bridge.statusHandler)
	group.POST("/purchases/lookup", bridge.lookupHandler)
	group.GET("/purchases/:itemId", bridge.getHandler)
	group.PUT("/purchases/:itemId", bridge.upsertHandler)
}

func odooAuthMiddleware(token string) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if token == "" {
			ctx.Next()
			return
		}
		got := ctx.GetHeader("X-Api-Key")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			ctx.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
			return
		}
		ctx.Next()
	}
}

func odooErrorResponse(ctx *gin.Context, err error) {
	var (
		v errValidation
		c errConflict
	)
	switch {
	case errors.As(err, &v):
		ctx.JSON(http.StatusBadRequest, gin.H{"error": v.msg})
	case errors.As(err, &c):
		ctx.JSON(http.StatusConflict, gin.H{"error": c.msg})
	default:
		log.Printf("odoo: %v", err)
		ctx.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
	}
}

// GET /odoo/status: prueba la conexión y el login.
func (b *OdooBridge) statusHandler(ctx *gin.Context) {
	version, err := b.odoo.Version()
	if err != nil {
		odooErrorResponse(ctx, err)
		return
	}
	if _, err := b.odoo.login(); err != nil {
		odooErrorResponse(ctx, err)
		return
	}
	ctx.JSON(http.StatusOK, gin.H{
		"connected":     true,
		"serverVersion": version["server_version"],
	})
}

// POST /odoo/purchases/lookup  {"itemIds": ["1234", ...]}
func (b *OdooBridge) lookupHandler(ctx *gin.Context) {
	var body struct {
		ItemIDs []string `json:"itemIds"`
	}
	if err := ctx.ShouldBindJSON(&body); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if len(body.ItemIDs) > maxLookupItems {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("at most %d itemIds", maxLookupItems)})
		return
	}
	for _, id := range body.ItemIDs {
		if !legacyItemIDRe.MatchString(id) {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid itemId: " + id})
			return
		}
	}

	items, err := b.Lookup(body.ItemIDs)
	if err != nil {
		odooErrorResponse(ctx, err)
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"items": items})
}

// GET /odoo/purchases/:itemId
func (b *OdooBridge) getHandler(ctx *gin.Context) {
	itemID := ctx.Param("itemId")
	if !legacyItemIDRe.MatchString(itemID) {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid itemId"})
		return
	}
	st, err := b.status(itemID)
	if err != nil {
		odooErrorResponse(ctx, err)
		return
	}
	ctx.JSON(http.StatusOK, st)
}

// PUT /odoo/purchases/:itemId
func (b *OdooBridge) upsertHandler(ctx *gin.Context) {
	itemID := ctx.Param("itemId")
	if !legacyItemIDRe.MatchString(itemID) {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid itemId"})
		return
	}
	var req odooPurchaseRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	st, created, err := b.Upsert(itemID, req)
	if err != nil {
		odooErrorResponse(ctx, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	ctx.JSON(status, st)
}
