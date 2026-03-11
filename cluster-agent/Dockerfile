FROM golang:1.22-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o cluster-agent .

FROM gcr.io/distroless/static-debian12
COPY --from=build /app/cluster-agent /cluster-agent
ENTRYPOINT ["/cluster-agent"]
