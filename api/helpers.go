package api

import (
	"encoding/json"
	"net/http"
)

//this function writes a standardized JSON response
func JSONResponse (w httpResponseWriter, status int, data any){
	w.header().Set("Content-Type", "application/json")
	w.Writeheader(status)
	if data !=nil{
		_ =json.NewEncoder(w).Encode(data)
	}
}

//this other function writes a standardized error message(json)
func ErrorResponse(w httpResponseWriter, status int, data any){
	JSONResponse(w, status, map[string]string{"error": message})
}