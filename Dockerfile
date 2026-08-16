FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /purgarr .

FROM gcr.io/distroless/static-debian12
COPY --from=build /purgarr /purgarr
VOLUME /data
ENTRYPOINT ["/purgarr"]
