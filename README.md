# Ebay Seller App Backend

## Endpoints

    GET /listings?seller=abc&sort=endTimeSoonest

`sort` accepts `endTimeSoonest` (alias `endingSoonest`), `price`, `-price` and
`newlyListed`. Any other value is not sent to eBay.
    GET /healtz
    