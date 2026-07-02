# 构建阶段
FROM golang:1.26-alpine AS builder

ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bridge ./cmd/bridge

# 运行阶段
FROM alpine:3.20

# chromedp 需要 chromium(谷歌搜索用)
RUN apk add --no-cache ca-certificates chromium nss freetype harfbuzz ttf-freefont

ENV CHROME_BIN=/usr/bin/chromium-browser
COPY --from=builder /bridge /usr/local/bin/bridge

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/bridge"]
