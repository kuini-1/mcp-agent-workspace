#!/bin/sh

name="$1"
if [ -z "$name" ]; then
  echo "No name provided. Please provide a name as an argument."
else
  echo "Hello, $name! Welcome to our system."
fi
