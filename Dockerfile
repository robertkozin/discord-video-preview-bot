# syntax=docker/dockerfile:1

FROM golang:alpine as builder

WORKDIR /app

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY *.go ./

RUN go build -o main

FROM alpine:edge

ARG CACHEBUST=1

RUN apk --no-cache add  -dlp ffmpeg python3

WORKDIR /app

COPY --from=builder /app /app

CMD ["./main"]
