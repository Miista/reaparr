FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /watch-cleanup-tool .

FROM gcr.io/distroless/static-debian12
COPY --from=build /watch-cleanup-tool /watch-cleanup-tool
VOLUME /data
ENTRYPOINT ["/watch-cleanup-tool"]
