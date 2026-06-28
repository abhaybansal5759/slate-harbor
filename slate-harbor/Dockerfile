# ---- build stage ----
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/server ./cmd/server

# ---- run stage ----
FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]