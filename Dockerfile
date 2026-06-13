FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /telegram-alerter .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /telegram-alerter /telegram-alerter
EXPOSE 8080
ENTRYPOINT ["/telegram-alerter"]
