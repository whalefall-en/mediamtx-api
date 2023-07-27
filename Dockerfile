FROM golang:1.20-alpine3.17

RUN apk add --no-cache \
    ffmpeg #下载ffmpeg

WORKDIR /s

COPY go.mod go.sum ./
RUN go mod download #下载依赖

COPY . ./
RUN go build -o /mediamtx . #编译

COPY start.sh /
RUN chmod +x /start.sh #赋予执行权限

ENTRYPOINT [ "/start.sh" ]
