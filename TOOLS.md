# Available Tools

CLI tools available for fulfilling user requests. Use these via shell execution.
All tools are installed and ready to use.

## Google Workspace — `gog`

Full Google Workspace CLI. Always use `--json --no-input --force` flags for scripting.
Account: pikamini.golden@gmail.com

### Google Docs

```bash
# Create a new doc
gog docs create "Title" --json

# Write content (replaces body)
gog docs write <docId> "markdown or plain text"

# Append content at a position
gog docs insert <docId> "text" --end

# Read a doc as plain text
gog docs cat <docId>

# Find and replace
gog docs find-replace <docId> "old" "new"

# Get metadata (title, id, url)
gog docs info <docId> --json
```

### Google Sheets

```bash
# Create a new spreadsheet
gog sheets create "Title" --json

# Read a range
gog sheets get <spreadsheetId> "Sheet1!A1:D10" --json

# Write to a range (values are positional args)
gog sheets update <spreadsheetId> "Sheet1!A1" "val1" "val2" "val3"

# Append rows
gog sheets append <spreadsheetId> "Sheet1!A1" "col1" "col2" "col3"

# Format cells
gog sheets format <spreadsheetId> "Sheet1!A1:D1" --bold --bg-color=lightblue

# Get spreadsheet metadata
gog sheets metadata <spreadsheetId> --json
```

### Google Slides

```bash
# Create a presentation
gog slides create "Title" --json

# Create from markdown (headings become slides)
gog slides create-from-markdown "Title" --file slides.md --json

# Add a slide with image
gog slides add-slide <presentationId> --image-url "url" --notes "speaker notes"

# List slides
gog slides list-slides <presentationId> --json
```

### Google Drive

```bash
# List files
gog drive ls --json

# Search files
gog drive search "query" --json

# Upload a file
gog drive upload localfile.pdf --json

# Share with someone
gog drive share <fileId> --email user@gmail.com --role writer

# Get shareable URL
gog drive url <fileId>

# Create a folder
gog drive mkdir "Folder Name" --json
```

### Gmail

```bash
# Send an email
gog send --to "user@email.com" --subject "Subject" --body "Body text" --no-input

# Search threads
gog gmail search "query" --json -n 5

# Read a thread
gog gmail thread get <threadId> --json
```

### Google Calendar

```bash
# List upcoming events
gog calendar events --json

# Create an event
gog calendar create "Meeting" --start "2026-03-01T10:00" --end "2026-03-01T11:00" --json

# Search events
gog calendar search "query" --json
```

### Google Tasks

```bash
# List tasks
gog tasks list --json

# Add a task
gog tasks add "Task description" --json

# Complete a task
gog tasks done <taskId>
```

### Google Forms

```bash
# Create a form
gog forms create "Form Title" --json

# Get form details
gog forms get <formId> --json

# List responses
gog forms responses list <formId> --json
```

---

## Web Search — `ddgr`

DuckDuckGo search from the command line. Use `--json` for structured output.

```bash
# Search and get JSON results
ddgr --json -n 5 "search query"

# Search a specific site
ddgr --json -n 5 -w example.com "query"

# Search with time filter (d=day, w=week, m=month, y=year)
ddgr --json -n 5 -t m "recent topic"

# Search a specific region
ddgr --json -n 5 -r us-en "query"
```

---

## HTTP Clients — `httpie` / `wget` / `curl`

```bash
# httpie — human-friendly HTTP client (auto JSON, colored output)
http GET "https://api.example.com/data"
http POST "https://api.example.com/items" name="Test" count:=5
http --download "https://example.com/file.pdf"

# wget — recursive downloads and mirroring
wget -q "https://example.com/file.pdf" -O output.pdf
wget -r -l 1 --no-parent "https://example.com/docs/"

# curl — raw HTTP requests
curl -sL "https://api.example.com/data" | jq '.results'
curl -sX POST "https://api.example.com" -H "Content-Type: application/json" -d '{"key":"val"}'
```

---

## Web Content Extraction — `lynx` / `pup`

Extract readable text or parse HTML structure from web pages.

```bash
# Extract readable text from a URL
curl -sL "https://example.com/article" | lynx -stdin -dump -nolist

# Extract with links preserved
curl -sL "https://example.com/article" | lynx -stdin -dump

# pup — CSS selector-based HTML parsing
# Binary: $(go env GOPATH)/bin/pup
curl -sL "https://example.com" | $(go env GOPATH)/bin/pup 'h2 text{}'
curl -sL "https://example.com" | $(go env GOPATH)/bin/pup 'a attr{href}'
curl -sL "https://example.com" | $(go env GOPATH)/bin/pup '.article-body p text{}'
curl -sL "https://example.com" | $(go env GOPATH)/bin/pup 'table json{}'
```

---

## Browser Automation — `shot-scraper`

Headless browser for screenshots, JavaScript execution, and page interaction.
Binary: `/Users/pikamini/Library/Python/3.9/bin/shot-scraper`

```bash
SHOT=/Users/pikamini/Library/Python/3.9/bin/shot-scraper

# Take a screenshot of a page
$SHOT "https://example.com" -o screenshot.png

# Take a screenshot of a specific element
$SHOT "https://example.com" -s "#main-content" -o element.png

# Execute JavaScript on a page and get the result
$SHOT javascript "https://example.com" "document.title"

# Execute JS that returns structured data
$SHOT javascript "https://example.com" "
  JSON.stringify(Array.from(document.querySelectorAll('h2')).map(h => h.textContent))
"

# Wait for an element before screenshot
$SHOT "https://example.com" --wait-for "#loaded" -o screenshot.png

# Full-page screenshot
$SHOT "https://example.com" --full-page -o full.png

# Set viewport size
$SHOT "https://example.com" --width 1280 --height 720 -o screenshot.png

# Use a specific browser (chromium is default)
$SHOT "https://example.com" --browser firefox -o screenshot.png
```

---

## Data Processing — `jq` / `mlr` / `csvkit`

```bash
# jq — JSON processor
echo '{"a":1}' | jq '.'
jq '.results[].name' data.json
jq '[.items[] | {title: .name, url: .link}]' data.json
jq -r '.rows[] | [.col1, .col2] | @tsv' data.json

# mlr (miller) — CSV/TSV/JSON swiss army knife
mlr --csv sort-by name data.csv
mlr --csv filter '$price > 10' data.csv
mlr --csv stats1 -a mean -f price data.csv
mlr --json put '$total = $price * $qty' data.json
mlr --icsv --ojson cat data.csv           # CSV to JSON
mlr --ijson --ocsv cat data.json          # JSON to CSV

# csvkit — CSV toolkit
csvstat data.csv                          # column stats summary
csvgrep -c name -m "search" data.csv      # filter rows
csvsort -c price data.csv                 # sort by column
csvjoin -c id file1.csv file2.csv         # join CSVs
csvlook data.csv                          # pretty-print table
in2csv data.xlsx > data.csv               # Excel to CSV
csvjson data.csv                          # CSV to JSON
```

---

## Document Conversion — `pandoc`

Universal document converter between formats.

```bash
# Markdown to HTML
pandoc input.md -o output.html

# Markdown to PDF (requires LaTeX or wkhtmltopdf)
pandoc input.md -o output.pdf

# Markdown to DOCX
pandoc input.md -o output.docx

# HTML to Markdown
pandoc -f html -t markdown "https://example.com/page" -o output.md
curl -sL "https://example.com" | pandoc -f html -t markdown

# DOCX to Markdown
pandoc input.docx -t markdown -o output.md

# Convert with template
pandoc input.md --template=template.html -o output.html
```

---

## Media — `ffmpeg` / `imagemagick` / `yt-dlp`

```bash
# ffmpeg — audio/video processing
ffmpeg -i input.mp4 -vn -acodec mp3 output.mp3        # extract audio
ffmpeg -i input.mp4 -ss 00:01:00 -t 30 clip.mp4       # cut 30s clip from 1:00
ffmpeg -i input.mp4 -vf "scale=720:-1" smaller.mp4     # resize video
ffmpeg -i input.mp4 -r 1 frame_%04d.png                # extract frames (1/sec)
ffmpeg -i input.wav -acodec libmp3lame output.mp3       # convert audio format

# imagemagick (magick) — image processing
magick input.png -resize 800x600 output.png            # resize
magick input.png -quality 80 output.jpg                # convert + compress
magick input.png -crop 400x300+100+50 cropped.png      # crop
magick montage *.png -geometry 200x200+2+2 grid.png    # create grid/montage
magick input.png -annotate +10+30 "Text" output.png    # add text overlay
magick identify input.png                               # get image info

# yt-dlp — download videos from YouTube and other sites
yt-dlp "https://youtube.com/watch?v=ID"                # download best quality
yt-dlp -x --audio-format mp3 "URL"                     # extract audio as MP3
yt-dlp --list-formats "URL"                             # list available formats
yt-dlp -f "bestvideo[height<=720]+bestaudio" "URL"     # max 720p
yt-dlp --write-subs --sub-lang en "URL"                # download with subtitles
yt-dlp --print title "URL"                              # just print title
```

---

## PDF Processing — `poppler`

```bash
# Extract text from PDF
pdftotext input.pdf -                                  # to stdout
pdftotext -layout input.pdf output.txt                 # preserve layout
pdftotext -f 1 -l 5 input.pdf -                       # pages 1-5

# Get PDF info
pdfinfo input.pdf

# Convert PDF pages to images
pdftoppm -png -r 150 input.pdf output                  # all pages as PNG
pdftoppm -png -f 1 -l 1 input.pdf cover               # first page only

# Extract images from PDF
pdfimages -png input.pdf images/
```

---

## Translation — `trans`

Google Translate from the command line.

```bash
# Auto-detect and translate to English
trans "Bonjour le monde"

# Translate to a specific language
trans :zh "Hello world"                                # to Chinese
trans :ja "Hello world"                                # to Japanese
trans :th "Hello world"                                # to Thai

# Translate between specific languages
trans en:zh "Hello world"

# Brief mode (translation only, no extras)
trans -b "Bonjour le monde"

# Translate a file
trans -b :en -i input.txt
```

---

## QR Codes — `qrencode`

```bash
# Generate QR code as PNG
qrencode -o qr.png "https://example.com"

# With custom size
qrencode -o qr.png -s 10 "text content"

# Output to terminal (UTF8)
qrencode -t UTF8 "https://example.com"
```

---

## Combining Tools — Common Workflows

### Research and summarize into a Google Doc

```bash
# 1. Search for information
ddgr --json -n 5 "topic to research"

# 2. Fetch top results
curl -sL "https://result-url.com" | lynx -stdin -dump -nolist

# 3. Create a Google Doc with findings
gog docs create "Research: Topic" --json
# → returns docId

# 4. Write the summary
gog docs write <docId> "# Research Summary\n\n..."

# 5. Share the doc
gog drive share <docId> --email user@gmail.com --role reader

# 6. Get the link to send back
gog drive url <docId>
```

### Build a comparison spreadsheet

```bash
# 1. Create spreadsheet
gog sheets create "Comparison: X vs Y" --json
# → returns spreadsheetId

# 2. Write headers
gog sheets update <spreadsheetId> "Sheet1!A1" "Name" "Price" "Rating" "Notes"

# 3. Append rows of data
gog sheets append <spreadsheetId> "Sheet1!A1" "Item 1" "$10" "4.5" "Good value"
gog sheets append <spreadsheetId> "Sheet1!A1" "Item 2" "$15" "4.8" "Premium"

# 4. Format header row
gog sheets format <spreadsheetId> "Sheet1!A1:D1" --bold

# 5. Share and get URL
gog drive share <spreadsheetId> --email user@gmail.com --role writer
gog drive url <spreadsheetId>
```

### Screenshot a page and email it

```bash
SHOT=/Users/pikamini/Library/Python/3.9/bin/shot-scraper

# 1. Take screenshot
$SHOT "https://example.com/page" -o /tmp/screenshot.png

# 2. Upload to Drive
gog drive upload /tmp/screenshot.png --json
# → returns fileId

# 3. Share and get URL
gog drive url <fileId>
```

---

## Notes

- All `gog` commands support `--json` for structured output, `--no-input` to never prompt, and `--force` to skip confirmations.
- When creating Google artifacts, always return the shareable URL to the user.
- For `gog` write operations on docs: content supports plain text. For rich formatting, create the doc then use `gog docs insert` or `gog docs find-replace`.
- `ddgr` returns JSON with `title`, `url`, and `abstract` fields.
- `shot-scraper` needs its full path: `/Users/pikamini/Library/Python/3.9/bin/shot-scraper`
- `pup` needs its full path: `$(go env GOPATH)/bin/pup` (resolves to `/Users/pikamini/dev/go/bin/pup`)
- Prefer `--json` output from all tools and parse with `jq` or `mlr` when chaining operations.
- Use `pandoc` to convert between document formats before uploading to Google Drive.
- Use `trans -b` for brief translation output suitable for piping.
