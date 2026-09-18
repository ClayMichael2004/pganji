package api

import (
	"encoding/json"
	"net/http"
)

//this function writes a standardized JSON response
func JSONResponse (w http.ResponseWriter, status int, data any){
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data !=nil{
		_ =json.NewEncoder(w).Encode(data)
	}
}

//this other function writes a standardized error message(json)
func ErrorResponse(w http.ResponseWriter, status int, message string){
	JSONResponse(w, status, map[string]string{"error": message})
}