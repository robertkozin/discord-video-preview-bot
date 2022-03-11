# syntax=docker/dockerfile:1

FROM golang:alpine

WORKDIR /app

RUN apk add ffmpeg python3 curl

RUN curl -L https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp \
    && chmod a+rx /usr/local/bin/yt-dlp

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY *.go ./

RUN go build -o main

CMD ["./main"]
