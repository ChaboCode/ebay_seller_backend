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
    