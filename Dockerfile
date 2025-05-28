FROM golang:alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY ./web ./web
COPY *.go ./
COPY downloader/*.go ./downloader

RUN CGO_ENABLED=0 go build -o main

FROM alpine

USER nobody

WORKDIR /static

WORKDIR /app
COPY --from=builder /app/main .

EXPOSE 8080

CMD ["./main"]
