# Ebay Seller App Backend

## Endpoints

    GET /listings?seller=abc&sort=endTimeSoonest

`sort` accepts `endTimeSoonest` (alias `endingSoonest`), `price`, `-price` and
`newlyListed`. Any other value is not sent to eBay.

    GET /image-proxy?url=<url-encoded eBay image URL>

Fetches an image from `*.ebayimg.com` / `*.ebaystatic.com` server-side and
forwards it. Any other host returns `400`.

    GET /healtz
    