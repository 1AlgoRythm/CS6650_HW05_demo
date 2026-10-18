# Product API server (HW5).
#
#   docker build --platform linux/amd64 -t product-service .
#   docker run --rm -p 8080:8080 product-service
#
# Stage 1 compiles the program. Stage 2 is a small image that holds only the program.
FROM golang:1.27.2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY product-service.go ./
# A static Linux program for x86_64. The ECS task definition sets no CPU type,
# so Fargate runs x86_64.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/server .

FROM alpine:3
COPY --from=build /out/server /server
# Quiet Gin's debug banner. The server listens on port 8080.
ENV GIN_MODE=release
EXPOSE 8080
CMD ["/server"]
