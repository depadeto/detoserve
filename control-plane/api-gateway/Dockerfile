FROM golang:1.22-alpine AS build
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o api-gateway .

FROM gcr.io/distroless/static-debian12
COPY --from=build /app/api-gateway /api-gateway
ENTRYPOINT ["/api-gateway"]
