FROM alpine:latest as certs
RUN apk --update add ca-certificates
FROM golang:1.22.2 AS build
RUN mkdir /app
WORKDIR /app
ADD . /app
RUN go mod download
RUN GOOS=linux GOARCH=amd64 go build -o ./build/bin -ldflags "-s -w"
# RUN ldd /app/bin | tr -s [:blank:] '\n' | grep ^/ | xargs -I % install -D % /app/%
# RUN ln -s ld-musl-x86_64.so.1 /app/lib/libc.musl-x86_64.so.1
FROM alpine:latest
RUN apk add --no-cache ffmpeg
RUN apk add --no-cache libc6-compat
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /app/build/bin /app/bin
ENTRYPOINT ["/app/bin"]