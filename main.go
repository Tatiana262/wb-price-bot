// main.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// --- Структуры для парсинга JSON ответа от WB API ---

type PriceInfo struct {
	Product   int `json:"product"`
	Logistics int `json:"logistics"`
}

type Size struct {
	Name   string        `json:"name"`
	Stocks []interface{} `json:"stocks"`
	Price  *PriceInfo    `json:"price"`
}

// --- ИСПРАВЛЕНО: Возвращаю структуру Color, как вы и хотели ---
type Color struct {
	Name string `json:"name"`
	ID   int    `json:"id"`
}

type Product struct {
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Sizes  []Size  `json:"sizes"`
	Colors []Color `json:"colors"` // --- ИСПРАВЛЕНО: Возвращаю поле Colors ---
}

type ProductData struct {
	Products []Product `json:"products"`
}

// --- НАША ГЛАВНАЯ СТРУКТУРА ДЛЯ ХРАНЕНИЯ ---

type TrackedItem struct {
	ProductName    string             `json:"productName"`
	RequestedSizes map[string]bool    `json:"requestedSizes"`
	LastPrices     map[string]float64 `json:"lastPrices"`
}

// --- Хранилище отслеживаемых товаров ---
var trackingData = make(map[int64]map[string]TrackedItem)
var mu sync.RWMutex
const dataFileName = "tracking.json"

// --- ФУНКЦИИ СОХРАНЕНИЯ И ЗАГРУЗКИ (остаются без изменений) ---

func saveDataToFile() error {
	mu.RLock()
	defer mu.RUnlock()
	dataBytes, err := json.MarshalIndent(trackingData, "", "  ")
	if err != nil { return fmt.Errorf("ошибка при маршалинге данных в JSON: %w", err) }
	err = ioutil.WriteFile(dataFileName, dataBytes, 0644)
	if err != nil { return fmt.Errorf("ошибка при записи данных в файл: %w", err) }
	log.Println("Данные успешно сохранены в", dataFileName)
	return nil
}

func loadDataFromFile() error {
	mu.Lock()
	defer mu.Unlock()
	dataBytes, err := ioutil.ReadFile(dataFileName)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("Файл данных не найден, начинаем с чистого листа.")
			trackingData = make(map[int64]map[string]TrackedItem)
			return nil
		}
		return fmt.Errorf("ошибка при чтении файла данных: %w", err)
	}
	if len(dataBytes) == 0 {
		log.Println("Файл данных пуст, начинаем с чистого листа.")
		trackingData = make(map[int64]map[string]TrackedItem)
		return nil
	}
	err = json.Unmarshal(dataBytes, &trackingData)
	if err != nil { return fmt.Errorf("ошибка при анмаршалинге JSON в данные: %w", err) }
	log.Println("Данные успешно загружены из", dataFileName)
	return nil
}

// --- КЛИЕНТ ДЛЯ API WILDBERRIES (остается без изменений) ---

func getWBProductInfo(article string) (*Product, error) {
	url := fmt.Sprintf("https://card.wb.ru/cards/v4/detail?appType=1&curr=byn&dest=-8144334&spp=30&nm=%s", article)
	client := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil { return nil, fmt.Errorf("ошибка при создании запроса: %w", err) }

	req.Header.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36")
	req.Header.Add("Referer", fmt.Sprintf("https://www.wildberries.by/catalog/%s/detail.aspx", article))

	resp, err := client.Do(req)
	if err != nil { return nil, fmt.Errorf("ошибка при выполнении запроса к WB API: %w", err) }
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("WB API вернул статус: %s, тело ответа: %s", resp.Status, string(bodyBytes))
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil { return nil, fmt.Errorf("ошибка при чтении тела ответа: %w", err) }
	
	var apiResponse ProductData
	if err := json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&apiResponse); err != nil {
		return nil, fmt.Errorf("ошибка при декодировании JSON: %w. Ответ сервера был: %s", err, string(bodyBytes))
	}
	if len(apiResponse.Products) == 0 {
		return nil, fmt.Errorf("товар с артикулом %s не найден", article)
	}
	return &apiResponse.Products[0], nil
}

func calculatePrice(price PriceInfo) float64 {
	return float64(price.Product+price.Logistics) / 100.0
}

// --- ОБРАБОТЧИКИ КОМАНД ---

func handleTrackingRequest(bot *tgbotapi.BotAPI, chatID int64, text string) {
	args := strings.Fields(text)
	if len(args) == 0 {
		bot.Send(tgbotapi.NewMessage(chatID, "Укажите артикул. Например: /track 123456 38 39"))
		return
	}
	article := args[0]
	if _, err := strconv.Atoi(article); err != nil {
		bot.Send(tgbotapi.NewMessage(chatID, "Артикул должен быть числом."))
		return
	}
	requestedSizes := make(map[string]bool)
	if len(args) > 1 {
		for _, size := range args[1:] {
			requestedSizes[size] = true
		}
	}
	bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Проверяю информацию по артикулу %s...", article)))
	product, err := getWBProductInfo(article)
	if err != nil {
		bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Не удалось получить информацию о товаре: %s", err.Error())))
		return
	}

	newItem := TrackedItem{
		ProductName:    product.Name + " " + product.Colors[0].Name,
		RequestedSizes: requestedSizes,
		LastPrices:     make(map[string]float64),
	}

	var responseText strings.Builder
	trackAll := len(requestedSizes) == 0
	if !trackAll {
		responseText.WriteString(fmt.Sprintf("Начинаю отслеживать *выбранные размеры* для товара: *%s*\n\n", newItem.ProductName))
	} else {
		responseText.WriteString(fmt.Sprintf("Начинаю отслеживать *все размеры* для товара: *%s*\n\n", newItem.ProductName))
	}

	// --- ИСПРАВЛЕНО: Логика формирования ответного сообщения ---
	for _, size := range product.Sizes {
		var currentPrice float64
		var inStock bool

		if len(size.Stocks) > 0 && size.Price != nil {
			currentPrice = calculatePrice(*size.Price)
			inStock = true
		}
		// Всегда сохраняем в нашу базу последнюю цену для ВСЕХ размеров
		newItem.LastPrices[size.Name] = currentPrice
		
		// А вот в ответное сообщение добавляем, только если это запрошенный размер (или если отслеживаем все)
		if trackAll || requestedSizes[size.Name] {
			if inStock {
				responseText.WriteString(fmt.Sprintf("Размер *%s*: `%.2f BYN`\n", size.Name, currentPrice))
			} else {
				responseText.WriteString(fmt.Sprintf("Размер *%s*: `нет в наличии`\n", size.Name))
			}
		}
	}

	mu.Lock()
	if _, ok := trackingData[chatID]; !ok {
		trackingData[chatID] = make(map[string]TrackedItem)
	}
	trackingData[chatID][article] = newItem
	mu.Unlock()

	responseText.WriteString("\nЯ сообщу об изменениях.")
	msg := tgbotapi.NewMessage(chatID, responseText.String())
	msg.ParseMode = "Markdown"
	bot.Send(msg)
	if err := saveDataToFile(); err != nil {
		log.Printf("ОШИБКА: не удалось сохранить данные: %v", err)
	}
}

// Остальные обработчики (`handleListRequest`, `handleUntrackRequest`) и `startPriceChecker` уже были правильными и остаются без изменений.
// Я оставляю их здесь для полноты файла.

func handleListRequest(bot *tgbotapi.BotAPI, chatID int64) {
	mu.RLock()
	defer mu.RUnlock()
	userTrackingData, ok := trackingData[chatID]
	if !ok || len(userTrackingData) == 0 {
		bot.Send(tgbotapi.NewMessage(chatID, "Вы пока не отслеживаете ни одного товара."))
		return
	}
	var responseText strings.Builder
	responseText.WriteString("Вы отслеживаете следующие товары:\n\n")
	for article, item := range userTrackingData {
		responseText.WriteString(fmt.Sprintf("✅ *Товар:* %s\n*Артикул:* `%s`\n", item.ProductName,article))
		
		for sizeName, price := range item.LastPrices {
			if item.RequestedSizes[sizeName] {
				if price == 0.0 {
					responseText.WriteString(fmt.Sprintf(" - Размер *%s*: `нет в наличии`\n", sizeName))
				} else {
					responseText.WriteString(fmt.Sprintf(" - Размер *%s*: `%.2f BYN`\n", sizeName, price))
				}
			}	
		}
		responseText.WriteString("\n")
	}
	msg := tgbotapi.NewMessage(chatID, responseText.String())
	msg.ParseMode = "Markdown"
	bot.Send(msg)
}

func handleUntrackRequest(bot *tgbotapi.BotAPI, chatID int64, article string) {
	article = strings.TrimSpace(article)
	if article == "" {
		bot.Send(tgbotapi.NewMessage(chatID, "Укажите артикул. Например: /untrack 123456"))
		return
	}
	var foundAndDeleted bool
	mu.Lock()
	if userTracking, ok := trackingData[chatID]; ok {
		if _, ok := userTracking[article]; ok {
			delete(userTracking, article)
			foundAndDeleted = true
		}
	}
	mu.Unlock()

	if foundAndDeleted {
		bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Больше не отслеживаю товар с артикулом %s.", article)))
		if err := saveDataToFile(); err != nil {
			log.Printf("ОШИБКА: не удалось сохранить данные после удаления товара: %v", err)
		}
	} else {
		bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Вы и не отслеживали товар с артикулом %s.", article)))
	}
}

func startPriceChecker(bot *tgbotapi.BotAPI) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		<-ticker.C
		log.Println("Запущена периодическая проверка цен...")
		mu.RLock()
		currentTracking := make(map[int64]map[string]TrackedItem)
		for chatID, articles := range trackingData {
			currentTracking[chatID] = make(map[string]TrackedItem)
			for article, item := range articles {
				currentTracking[chatID][article] = item
			}
		}
		mu.RUnlock()

		for chatID, articles := range currentTracking {
			for article, oldItem := range articles {
				newProductInfo, err := getWBProductInfo(article)
				if err != nil {
					log.Printf("Ошибка проверки артикула %s: %v", article, err)
					continue
				}

				newSizesMap := make(map[string]Size)
				for _, s := range newProductInfo.Sizes {
					newSizesMap[s.Name] = s
				}
				
				var anyChangeHappened bool
				for sizeName, oldPrice := range oldItem.LastPrices {
					var message string
					var priceChanged bool
					newSize, newSizeExists := newSizesMap[sizeName]
					isNowInStock := newSizeExists && len(newSize.Stocks) > 0 && newSize.Price != nil
					wasInStock := oldPrice > 0.0

					if wasInStock && !isNowInStock {
						message = fmt.Sprintf("Товар *закончился* 😱\n\nТовар: *%s*\nАртикул: `%s`\nРазмер: *%s*", oldItem.ProductName, article, sizeName)
						mu.Lock()
						trackingData[chatID][article].LastPrices[sizeName] = 0.0
						mu.Unlock()
						priceChanged = true
					} else if !wasInStock && isNowInStock {
						newPrice := calculatePrice(*newSize.Price)
						message = fmt.Sprintf("*Снова в наличии!* ✅\n\nТовар: *%s*\nАртикул: `%s`\nРазмер: *%s*\n\nНовая цена: `%.2f BYN`", oldItem.ProductName, article, sizeName, newPrice)
						mu.Lock()
						trackingData[chatID][article].LastPrices[sizeName] = newPrice
						mu.Unlock()
						priceChanged = true
					} else if wasInStock && isNowInStock {
						newPrice := calculatePrice(*newSize.Price)
						if newPrice < oldPrice {
							message = fmt.Sprintf("❗️*Снижение цены!*\n\nТовар: *%s*\nАртикул: `%s`\nРазмер: *%s*\n\nСтарая цена: `%.2f BYN`\nНовая цена: `%.2f BYN`", oldItem.ProductName, article, sizeName, oldPrice, newPrice)
							mu.Lock()
							trackingData[chatID][article].LastPrices[sizeName] = newPrice
							mu.Unlock()
							priceChanged = true
						} else if newPrice != oldPrice {
							mu.Lock()
							trackingData[chatID][article].LastPrices[sizeName] = newPrice
							mu.Unlock()
							priceChanged = true
						}
					}
					trackThisSize := len(oldItem.RequestedSizes) == 0 || oldItem.RequestedSizes[sizeName]
					if message != "" && trackThisSize {
						log.Println("Найдено изменение:", message)
						msg := tgbotapi.NewMessage(chatID, message)
						msg.ParseMode = "Markdown"
						bot.Send(msg)
					}
					if priceChanged {
						anyChangeHappened = true
					}
				}
				if anyChangeHappened {
					if err := saveDataToFile(); err != nil {
						log.Printf("ОШИБКА: не удалось сохранить обновленную цену: %v", err)
					}
				}
				time.Sleep(2 * time.Second)
			}
		}
	}
}

// --- ОСНОВНАЯ ФУНКЦИЯ ---

func main() {
	if err := loadDataFromFile(); err != nil {
		log.Panicf("Критическая ошибка: не удалось загрузить данные: %v", err)
	}
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Panic("TELEGRAM_BOT_TOKEN не установлен!")
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Panic(err)
	}
	bot.Debug = true
	log.Printf("Авторизован как %s", bot.Self.UserName)
	go startPriceChecker(bot)
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)
	for update := range updates {
		if update.Message == nil { continue }
		chatID := update.Message.Chat.ID
		msgText := update.Message.Text
		if update.Message.IsCommand() {
			switch update.Message.Command() {
			case "start":
				reply := "Привет! Я бот для отслеживания цен на Wildberries.\n\n" +
					"Используй команды:\n" +
					"`/track [артикул] [размер1] [размер2]` - начать отслеживать товар. Если размеры не указаны, отслеживаются все.\n" +
					"`/list` - показать список отслеживаемых товаров.\n" +
					"`/untrack [артикул]` - прекратить отслеживание товара."
				msg := tgbotapi.NewMessage(chatID, reply)
				msg.ParseMode = "Markdown"
				bot.Send(msg)
			case "track":
				handleTrackingRequest(bot, chatID, update.Message.CommandArguments())
			case "list":
				handleListRequest(bot, chatID)
			case "untrack":
				handleUntrackRequest(bot, chatID, update.Message.CommandArguments())
			default:
				msg := tgbotapi.NewMessage(chatID, "Неизвестная команда. Используйте /start для помощи.")
				bot.Send(msg)
			}
		} else {
			handleTrackingRequest(bot, chatID, msgText)
		}
	}
}
