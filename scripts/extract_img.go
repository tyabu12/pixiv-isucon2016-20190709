package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"
)

var (
	envfile = flag.String("env", "./env.sh", "Env file")
	outpath = flag.String("o", "/tmp/icons", "Output directory")
)

func connectDb() (db *sqlx.DB, err error) {
	dbHost := os.Getenv("ISUCONP_DB_HOST")
	if dbHost == "" {
		dbHost = "127.0.0.1"
	}
	dbPort := os.Getenv("ISUCONP_DB_PORT")
	if dbPort == "" {
		dbPort = "3306"
	}
	dbUser := os.Getenv("ISUCONP_DB_USER")
	if dbUser == "" {
		dbUser = "root"
	}
	dbPassword := os.Getenv("ISUCONP_DB_PASSWORD")
	if dbPassword != "" {
		dbPassword = ":" + dbPassword
	}
	dbName := os.Getenv("ISUCONP_DB_NAME")
	if dbName != "" {
		dbName = "isuconp"
	}

	dsn := fmt.Sprintf(
		"%s%s@tcp(%s:%s)/%s?parseTime=true&loc=Local&charset=utf8mb4",
		dbUser, dbPassword, dbHost, dbPort, dbName)

	fmt.Printf("Connecting to db: %q\n", dsn)

	db, err = sqlx.Connect("mysql", dsn)
	if err != nil {
		return
	}

	for i := 0; i < 10; i++ {
		err = db.Ping()
		if err == nil {
			break
		}
		fmt.Println(err)
		time.Sleep(time.Second * 3)
	}
	if err != nil {
		return
	}

	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(1 * time.Minute)

	fmt.Println("Succeeded to connect db.")
	return
}

func extractImg(db *sqlx.DB) error {
	type Image struct {
		Id   string `db:"id"`
		Mime string `db:"mime"`
		Data []byte `db:"imgdata"`
	}

	fmt.Printf("Extracting icon images to %s\n", *outpath)

	offset, limit := 0, 1000
	for {
		images := []Image{}
		err := db.Select(&images, "SELECT `id`, `mime`, `imgdata` FROM `posts` LIMIT ? OFFSET ?", limit, offset)
		if err != nil {
			return err
		}
		if len(images) == 0 {
			break
		}

		// outpath 以下にファイル書き出し
		extMap := map[string]string{"image/jpeg": ".jpeg", "image/gif": ".gif", "image/png": ".png"}
		for _, image := range images {
			ext, ok := extMap[image.Mime]
			if !ok {
				return err
			}
			f, err := os.Create(*outpath + "/" + image.Id + ext)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.Write(image.Data)
			if err != nil {
				return err
			}
		}

		offset += limit
	}
	fmt.Println("Succeeded to extract icon images.")
	return nil
}

func main() {
	flag.Parse()

	if err := godotenv.Load(*envfile); err != nil {
		log.Fatalf("Loading .env file failed: %v\n", err)
	}

	os.Mkdir(*outpath, 0777)

	db, err := connectDb()
	if err != nil {
		log.Fatalln(err)
	}

	if err := extractImg(db); err != nil {
		log.Fatalln(err)
	}
}
