package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/go-chi/chi/v5"
)

type Product struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

var (
	index    bleve.Index
	products []Product
)

func generateProducts() []Product {
	rand.Seed(time.Now().UnixNano())
	brands := []string{"Nike", "Adidas", "Logitech", "Sony", "Apple", "Samsung", "Lenovo", "Ikea", "Fitbit", "HP"}
	items := []string{"Running Shoes", "Wireless Mouse", "Yoga Mat", "Gaming Keyboard", "Water Bottle", "Smart Watch", "Bluetooth Speaker", "Desk Lamp", "Laptop Stand", "Backpack"}
	colors := []string{"Black", "White", "Blue", "Red", "Green", "Gray", "Silver"}
	categories := map[string]string{
		"Running Shoes":     "Footwear",
		"Wireless Mouse":    "Electronics",
		"Yoga Mat":          "Fitness",
		"Gaming Keyboard":   "Electronics",
		"Water Bottle":      "Home & Kitchen",
		"Smart Watch":       "Electronics",
		"Bluetooth Speaker": "Electronics",
		"Desk Lamp":         "Home & Kitchen",
		"Laptop Stand":      "Accessories",
		"Backpack":          "Accessories",
	}

	out := make([]Product, 0, 1000000)
	for i := 1; i <= 1000000; i++ {
		item := items[rand.Intn(len(items))]
		brand := brands[rand.Intn(len(brands))]
		color := colors[rand.Intn(len(colors))]
		out = append(out, Product{
			ID:       i,
			Name:     fmt.Sprintf("%s %s – %s", brand, item, color),
			Category: categories[item],
		})
	}
	return out
}

func createIndex() error {
	log.Println("Starting to create index...")
	startTime := time.Now()

	indexMapping := bleve.NewIndexMapping()
	nameField := bleve.NewTextFieldMapping()
	nameField.Analyzer = "standard"
	categoryField := bleve.NewTextFieldMapping()
	categoryField.Analyzer = "standard"
	productMapping := bleve.NewDocumentMapping()
	productMapping.AddFieldMappingsAt("Name", nameField)
	productMapping.AddFieldMappingsAt("Category", categoryField)
	indexMapping.DefaultMapping = productMapping

	var err error
	index, err = bleve.NewUsing(
		"",
		indexMapping,
		bleve.Config.DefaultIndexType,
		bleve.Config.DefaultMemKVStore,
		nil,
	)
	if err != nil {
		return err
	}

	productIDs := make([]string, len(products))
	for i := range products {
		productIDs[i] = strconv.Itoa(products[i].ID)
	}

	const (
		batchSize   = 50000
		numWorkers  = 12
		queueBuffer = 100
	)
	var wg sync.WaitGroup
	batches := make(chan []int, queueBuffer)
	errChan := make(chan error, 1)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for indices := range batches {
				batch := index.NewBatch()
				for _, i := range indices {
					batch.Index(productIDs[i], products[i])
				}
				if berr := index.Batch(batch); berr != nil {
					select {
					case errChan <- berr:
					default:
					}
					return
				}
			}
		}()
	}

	allIndices := make([]int, len(products))
	for i := range allIndices {
		allIndices[i] = i
	}
	chunk := batchSize / numWorkers
	for start := 0; start < len(allIndices); start += chunk {
		end := start + chunk
		if end > len(allIndices) {
			end = len(allIndices)
		}
		select {
		case batches <- allIndices[start:end]:
		case e := <-errChan:
			close(batches)
			return e
		}
	}
	close(batches)
	wg.Wait()

	select {
	case e := <-errChan:
		return e
	default:
		elapsed := time.Since(startTime)
		log.Printf("Indexed %d products in %.2f seconds (%.2f docs/sec)",
			len(products), elapsed.Seconds(), float64(len(products))/elapsed.Seconds())
		return nil
	}
}

func searchProducts(q string) ([]Product, error) {
	startTime := time.Now()

	fuzzyQuery := bleve.NewFuzzyQuery(q)
	fuzzyQuery.Fuzziness = 2

	req := bleve.NewSearchRequest(fuzzyQuery)
	req.Size = 50
	req.Fields = []string{"*"}
	req.Highlight = bleve.NewHighlight()

	res, err := index.Search(req)
	if err != nil {
		return nil, err
	}
	log.Printf("Search for '%s' found %d hits in %.2fms", q, res.Total, time.Since(startTime).Seconds()*1000)

	var results []Product
	for _, hit := range res.Hits {
		id, _ := strconv.Atoi(hit.ID)
		if id > 0 && id <= len(products) {
			results = append(results, products[id-1])
		}
	}
	return results, nil
}

func main() {
	log.Println("Starting product search service...")
	products = generateProducts()
	log.Printf("Generated %d products", len(products))

	if err := createIndex(); err != nil {
		log.Fatalf("Failed to create index: %v", err)
	}

	r := chi.NewRouter()
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/index.html")
	})
	r.Get("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			http.Error(w, "q parameter required", http.StatusBadRequest)
			return
		}
		results, err := searchProducts(q)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Total-Count", strconv.Itoa(len(results)))
		json.NewEncoder(w).Encode(results)
	})
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		index.Close()
		os.Exit(0)
	}()

	log.Fatal(http.ListenAndServe(":8080", r))
}
