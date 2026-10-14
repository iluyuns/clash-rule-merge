FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /clash-rule-merge .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 app \
    && mkdir -p /data \
    && chown app /data
COPY --from=build /clash-rule-merge /usr/local/bin/clash-rule-merge
USER app
ENV LISTEN=:8080 \
    DATA_FILE=/data/subs.json \
    CACHE_DIR=/data/cache
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["clash-rule-merge"]
