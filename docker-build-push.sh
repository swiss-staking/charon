#!/bin/bash

repo=swissstaking/charon
# Read the version
version=$(cat docker-version.txt)

# Increment the version
IFS='.' read -ra parts <<< "$version"
((parts[2]++))
new_version="${parts[0]}.${parts[1]}.${parts[2]}"

# Save the incremented version back to the file
echo $new_version > docker-version.txt

# Build the Docker image
docker build -t $repo:$new_version .
docker push $repo:$new_version