FROM --platform=$BUILDPLATFORM golang:1.22.2 AS build
RUN mkdir /app
WORKDIR /app
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download
COPY . .
RUN GOOS=linux go build -o ./build/bin -ldflags "-s -w"
FROM --platform=$BUILDPLATFORM alpine:latest
RUN apk add --no-cache ffmpeg
RUN apk add --no-cache libc6-compat
COPY --from=build /app/build/bin /app/bin
ENTRYPOINT ["/app/bin"]