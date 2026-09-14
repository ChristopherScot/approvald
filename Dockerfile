# Use a minimal base image to run the application
FROM alpine:latest

# Set the working directory inside the container
WORKDIR /root/

# Copy the pre-built application into the container
COPY bin/app .

# Expose the port the application runs on
EXPOSE 3000

# Define the entry point to run the application
CMD ["./app"]