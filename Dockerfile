FROM golang:1.26.5-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/gline-server ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S gline \
    && adduser -S -G gline gline
COPY --from=build /out/gline-server /usr/local/bin/gline-server
USER gline
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gline-server"]
