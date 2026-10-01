package main

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // zona horaria embebida: evita depender de tzdata del sistema

	"github.com/gin-gonic/gin"
)

const defaultEndingTZ = "America/Mexico_City"

// auctionItem es el subconjunto de campos de item_summary que usa el endpoint.
type auctionItem struct {
	ItemID       string   `json:"itemId"`
	LegacyItemID string   `json:"legacyItemId"`
	Title        string   `json:"title"`
	ItemEndDate  string   `json:"itemEndDate"`
	ItemWebURL   string   `json:"itemWebUrl"`
	BuyingOption []string `json:"buyingOptions"`
	BidCount     int      `json:"bidCount"`
	Image        struct {
		ImageURL string `json:"imageUrl"`
	} `json:"image"`
	CurrentBidPrice struct {
		Value    string `json:"value"`
		Currency string `json:"currency"`
	} `json:"currentBidPrice"`
}

// endingAuction es lo que devuelve GET /auctions/ending por cada subasta.
type endingAuction struct {
	ItemID         string `json:"itemId"`
	LegacyItemID   string `json:"legacyItemId"`
	Title          string `json:"title"`
	EndsAtUTC      string `json:"endsAtUtc"`
	EndsAtLocal    string `json:"endsAtLocal"`
	ImageURL       string `json:"imageUrl"`
	ImageURLLarge  string `json:"imageUrlLarge"`
	SuggestedName  string `json:"suggestedFilename"`
	ItemWebURL     string `json:"itemWebUrl"`
	BidCount       int    `json:"bidCount"`
	CurrentBid     string `json:"currentBid,omitempty"`
	CurrentBidCurr string `json:"currentBidCurrency,omitempty"`
}

var (
	ebayImageSizeRe = regexp.MustCompile(`s-l\d+\.`)
	unsafeFileChars = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)
	multiSpaceRe    = regexp.MustCompile(`\s+`)
)

// largeImageURL sube la resolución de una imagen de eBay (s-l225.jpg -> s-l1600.jpg).
func largeImageURL(u string) string {
	if u == "" {
		return ""
	}
	return ebayImageSizeRe.ReplaceAllString(u, "s-l1600.")
}

// imageExt devuelve la extensión (con punto) de la URL de imagen, o ".jpg".
func imageExt(u string) string {
	if parsed, err := parseURLPath(u); err == nil {
		for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp", ".gif"} {
			if strings.HasSuffix(strings.ToLower(parsed), ext) {
				return ext
			}
		}
	}
	return ".jpg"
}

func parseURLPath(u string) (string, error) {
	i := strings.IndexAny(u, "?#")
	if i >= 0 {
		u = u[:i]
	}
	return u, nil
}

// safeFilename convierte un título de eBay en un nombre de archivo válido para
// Drive/Windows/macOS: quita caracteres reservados, colapsa espacios, recorta
// a maxLen runas y agrega el itemId para evitar colisiones entre títulos iguales.
func safeFilename(title, itemID, ext string) string {
	const maxLen = 100
	name := unsafeFileChars.ReplaceAllString(title, " ")
	name = multiSpaceRe.ReplaceAllString(name, " ")
	name = strings.Trim(strings.TrimSpace(name), ". ")
	if r := []rune(name); len(r) > maxLen {
		name = strings.TrimSpace(string(r[:maxLen]))
	}
	if name == "" {
		name = "sin-titulo"
	}
	if itemID != "" {
		name += " - " + itemID
	}
	return name + ext
}

func hasBuyingOption(opts []string, want string) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}

// dayRange devuelve [inicio, fin) del día calendario `day` en la zona loc.
// day puede ser "" / "tomorrow", "today" o "YYYY-MM-DD".
func dayRange(day string, loc *time.Location, now time.Time) (time.Time, time.Time, error) {
	local := now.In(loc)
	base := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

	var start time.Time
	switch strings.ToLower(day) {
	case "", "tomorrow", "manana", "mañana":
		start = base.AddDate(0, 0, 1)
	case "today", "hoy":
		start = base
	default:
		d, err := time.ParseInLocation("2006-01-02", day, loc)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		start = d
	}
	// AddDate sobre fecha local respeta cambios de horario (DST).
	return start, start.AddDate(0, 0, 1), nil
}

// EndingAuctionsHandler lista las subastas de un vendedor que cierran en un
// día calendario (por defecto, mañana en hora de México), con título e
// imagen principal listos para descargar.
//
//	GET /auctions/ending?seller=abc&date=tomorrow|today|YYYY-MM-DD&tz=America/Mexico_City
func EndingAuctionsHandler(client *Client) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		seller := ctx.DefaultQuery("seller", os.Getenv("DEFAULT_SELLER_USERNAME"))
		if seller == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "seller is required"})
			return
		}

		tzName := ctx.DefaultQuery("tz", defaultEndingTZ)
		loc, err := time.LoadLocation(tzName)
		if err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid tz: " + tzName})
			return
		}

		start, end, err := dayRange(ctx.Query("date"), loc, time.Now())
		if err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid date, use tomorrow, today or YYYY-MM-DD"})
			return
		}

		// Reutiliza la búsqueda multi-marketplace (deduplicada, ordenada por
		// cierre y con caché de 60 s) y filtra aquí.
		items, err := client.SearchSellerAllMarketplaces(seller, "endingSoonest")
		if err != nil {
			ctx.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}

		auctions := make([]endingAuction, 0)
		for _, raw := range items {
			var it auctionItem
			if err := json.Unmarshal(raw, &it); err != nil {
				continue
			}
			if !hasBuyingOption(it.BuyingOption, "AUCTION") || it.ItemEndDate == "" {
				continue
			}
			endsAt, err := time.Parse(time.RFC3339, it.ItemEndDate)
			if err != nil {
				continue
			}
			if endsAt.Before(start) || !endsAt.Before(end) {
				continue
			}

			id := it.LegacyItemID
			if id == "" {
				id = it.ItemID
			}
			auctions = append(auctions, endingAuction{
				ItemID:         it.ItemID,
				LegacyItemID:   it.LegacyItemID,
				Title:          it.Title,
				EndsAtUTC:      endsAt.UTC().Format(time.RFC3339),
				EndsAtLocal:    endsAt.In(loc).Format(time.RFC3339),
				ImageURL:       it.Image.ImageURL,
				ImageURLLarge:  largeImageURL(it.Image.ImageURL),
				SuggestedName:  safeFilename(it.Title, id, imageExt(it.Image.ImageURL)),
				ItemWebURL:     it.ItemWebURL,
				BidCount:       it.BidCount,
				CurrentBid:     it.CurrentBidPrice.Value,
				CurrentBidCurr: it.CurrentBidPrice.Currency,
			})
		}

		ctx.JSON(http.StatusOK, gin.H{
			"seller":    seller,
			"timezone":  tzName,
			"rangeFrom": start.Format(time.RFC3339),
			"rangeTo":   end.Format(time.RFC3339),
			"total":     len(auctions),
			"auctions":  auctions,
		})
	}
}
