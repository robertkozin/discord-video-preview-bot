# syntax=docker/dockerfile:1

FROM golang:alpine as builder

WORKDIR /app

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY *.go ./

RUN go build -o main

FROM alpine:edge

RUN apk add --no-cache ffmpeg

WORKDIR /app

COPY --from=builder /app /app

CMD ["./main"]
