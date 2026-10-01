# Ebay Seller App Backend

## Endpoints

    GET /listings?seller=abc&sort=endTimeSoonest

`sort` accepts `endTimeSoonest` (alias `endingSoonest`), `price`, `-price` and
`newlyListed`. Any other value is not sent to eBay.

    GET /auctions/ending?seller=abc&date=tomorrow&tz=America/Mexico_City

Lists the seller's **auctions** (not fixed-price) that end on a calendar day.
`date` is `tomorrow` (default), `today` or `YYYY-MM-DD`; `tz` defaults to
`America/Mexico_City`. Each result includes `title`, `endsAtUtc`/`endsAtLocal`,
`imageUrl` (first image), `imageUrlLarge` (s-l1600) and `suggestedFilename`
(sanitised title + item id), ready to download and upload to Google Drive.

    GET /image-proxy?url=<url-encoded eBay image URL>

Fetches an image from `*.ebayimg.com` / `*.ebaystatic.com` server-side and
forwards it. Any other host returns `400`.

    GET /healtz

## Odoo (módulo de Compras)

Registra figuras de eBay como compras en Odoo (16, 17, 18 o 19) vía JSON-RPC.
Se activa con estas variables; sin ellas, `/odoo/*` responde `503`:

| Variable | Ejemplo |
| --- | --- |
| `ODOO_URL` | `https://mi-empresa.odoo.com` |
| `ODOO_DB` | nombre de la base de datos |
| `ODOO_USERNAME` | login del usuario de integración |
| `ODOO_API_KEY` | API key de ese usuario (Preferencias → Seguridad de la cuenta → Nueva API key). También acepta `ODOO_PASSWORD` |
| `ODOO_BRIDGE_TOKEN` | opcional: si está definido, `/odoo/*` exige el header `X-Api-Key` con este valor |

Odoo necesita los módulos **Compras** (`purchase`) e **Inventario** (`stock`),
y el usuario permisos de Compras (Administrador o Usuario) e Inventario.

Por cada figura se crea:

- **Proveedor**: el vendedor de eBay. Se busca, sin importar mayúsculas, por
  la *Referencia* del contacto (`sakura0418`) o por nombre (`sakura0418` /
  `@sakura0418`). Si no existe se crea como empresa `@sakura0418`, con
  referencia `sakura0418` y su página de eBay.
- **Producto** almacenable con referencia interna `EBAY-<legacyItemId>`, el
  título, la primera imagen (s-l1600) y el costo.
- **Solicitud de presupuesto** (RFQ en borrador) a ese proveedor, con una
  línea de 1 unidad al costo indicado, en la moneda del listing si está
  activa en Odoo. La referencia de proveedor y el documento origen son el id
  de eBay.

Luego, en Odoo, se confirma la RFQ y se valida la recepción para que la
figura entre al inventario.

    GET /odoo/status

Prueba la conexión y el login: `{"connected": true, "serverVersion": "18.0"}`.

    POST /odoo/purchases/lookup   {"itemIds": ["306512345678", ...]}

Estado de hasta 1000 figuras (por `legacyItemId`):
`{"items": {"306512345678": {...estado...}}}`.

    GET /odoo/purchases/:itemId

Estado de una figura:

```json
{
  "itemId": "306512345678", "inOdoo": true, "productId": 1,
  "orderId": 1, "orderName": "P00001", "state": "draft",
  "cost": 45.5, "currency": "USD", "qtyReceived": 0,
  "vendor": "@sakura0418", "editable": true,
  "odooUrl": "https://mi-empresa.odoo.com/web#id=1&model=purchase.order&view_type=form"
}
```

`state` es el de la orden de compra: `draft`, `sent`, `to approve`,
`purchase` (confirmada) o `done` (bloqueada). Las órdenes canceladas se
ignoran, así que la figura se puede volver a registrar.

    PUT /odoo/purchases/:itemId

```json
{
  "title": "Figura ...", "imageUrl": "https://i.ebayimg.com/...",
  "seller": "sakura0418", "cost": 45.5, "currency": "USD",
  "auctionPrice": 42.0, "listingUrl": "https://www.ebay.com/itm/..."
}
```

Si la figura no tiene orden, crea proveedor, producto y RFQ (`201`). Si ya
la tiene, solo actualiza el costo de la línea y del producto (`200`); con la
orden bloqueada responde `409`. Devuelve el estado, con `warnings` si no se
pudo adjuntar la imagen o la moneda no está activa.
    