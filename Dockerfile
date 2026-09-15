FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /harmonia ./cmd/harmonia

FROM build AS test
RUN apk add --no-cache ffmpeg build-base
RUN go test -race -cover ./... && go vet ./...

FROM alpine:3.23
RUN apk add --no-cache ffmpeg ca-certificates tzdata && mkdir /data
COPY --from=build /harmonia /usr/local/bin/harmonia
ENV HARMONIA_DATA=/data HARMONIA_LISTEN=:8090
VOLUME ["/data"]
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q -O /dev/null http://127.0.0.1:8090/api/health || exit 1
ENTRYPOINT ["harmonia"]
