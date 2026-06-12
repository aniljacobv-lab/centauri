# Centauri — single-binary, bi-temporal, AI-first event database.
# Build:  docker build -t centauri .
# Run:    docker run -p 7771:7771 -v centauri-data:/data centauri
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN go build -trimpath -ldflags "-s -w" -o /centauri ./cmd/centauri

FROM alpine:3.20
COPY --from=build /centauri /usr/local/bin/centauri
VOLUME /data
EXPOSE 7771
# The token can be set at runtime: -e CENTAURI_TOKEN=secret
ENTRYPOINT ["centauri"]
CMD ["serve", "-data", "/data/centauri.log", "-addr", ":7771"]
