package stat

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"hehan.net/my/stockcmd/logger"

	"gonum.org/v1/gonum/stat"

	"github.com/rocketlaunchr/dataframe-go"

	"github.com/fatih/color"
	"github.com/iancoleman/strcase"
	"github.com/jinzhu/now"
	"github.com/pkg/errors"
	"hehan.net/my/stockcmd/baostock"
	"hehan.net/my/stockcmd/sina"
	"hehan.net/my/stockcmd/store"
)

type DailyStat struct {
	Name         string
	Now          float64
	ChgToday     float64
	Last         float64
	ChgLast      float64
	ChgMonth     float64 `sc:"chg_m"`
	ChgLastMonth float64 `sc:"chg_lm"`
	ChgYear      float64 `sc:"chg_y"`
	Avg20        float64
	Avg60        float64
	Avg200       float64
	Chg20        float64
	Chg60        float64
	Chg90        float64
	Code         string
}

func Fields(s interface{}) []string {
	var fields []string
	v := reflect.ValueOf(s)
	for i := 0; i < v.NumField(); i++ {
		display := v.Type().Field(i).Tag.Get("sc")
		if len(display) == 0 {
			display = strcase.ToSnake(v.Type().Field(i).Name)
		}
		fields = append(fields, display)
	}
	return fields
}

func Float64String(f float64) string {
	return fmt.Sprintf("%.2f", f)
}

func ChgString(chg float64) string {
	chgStr := Float64String(chg)
	post := ""
	switch {
	case chg > 3:
		post = "✨"
	case chg > 0:
		post = "↑"
	case chg == 0:
		post = "⁃"
	case chg < -3:
		post = "⚡"
	case chg < 0:
		post = "↓"
	}
	chgStr = fmt.Sprintf("%s %s", chgStr, post)
	if chg >= 3.0 {
		chgStr = color.RedString(chgStr)
	} else if chg <= -3.0 {
		chgStr = color.GreenString(chgStr)
	}
	return chgStr
}

func (ds *DailyStat) Row() []string {
	row := make([]string, 0, 32)

	nameStr := ds.Name
	if ds.Avg20 > ds.Now {
		nameStr = color.BlueString(ds.Name)
	}
	if ds.Avg60 > ds.Now {
		nameStr = color.GreenString(ds.Name)
	}
	if ds.Avg200 > ds.Now {
		nameStr = color.HiGreenString(ds.Name)
	}
	row = append(row, nameStr)
	row = append(row, Float64String(ds.Now))
	row = append(row, ChgString(ds.ChgToday))
	row = append(row, Float64String(ds.Last))
	row = append(row, ChgString(ds.ChgLast))
	row = append(row, Float64String(ds.ChgMonth))
	row = append(row, Float64String(ds.ChgLastMonth))
	row = append(row, Float64String(ds.ChgYear))
	row = append(row, Float64String(ds.Avg20))
	row = append(row, Float64String(ds.Avg60))
	row = append(row, Float64String(ds.Avg200))
	row = append(row, Float64String(ds.Chg20))
	row = append(row, Float64String(ds.Chg60))
	row = append(row, Float64String(ds.Chg90))
	row = append(row, ds.Code)

	return row
}

func thisMonthFilterFn(vals map[interface{}]interface{}, row, nRows int) (dataframe.FilterAction, error) {
	now := time.Now()
	month := now.Month()
	year := now.Year()
	date := vals["date"].(time.Time)
	if date.Month() == month && date.Year() == year {
		return dataframe.KEEP, nil
	} else {
		return dataframe.DROP, nil
	}
}

func lastMonthFilterFn(vals map[interface{}]interface{}, row, nRow int) (dataframe.FilterAction, error) {
	now := time.Now()
	lastNow := now.AddDate(0, -1, 0)
	year := lastNow.Year()
	month := lastNow.Month()

	date := vals["date"].(time.Time)
	if date.Month() == month && date.Year() == year {
		return dataframe.KEEP, nil
	} else {
		return dataframe.DROP, nil
	}
}

func thisYearFilterFn(vals map[interface{}]interface{}, row, nRow int) (dataframe.FilterAction, error) {
	year := time.Now().Year()

	date := vals["date"].(time.Time)
	if date.Year() == year {
		return dataframe.KEEP, nil
	} else {
		return dataframe.DROP, nil
	}
}

func chgWithDf(df *dataframe.DataFrame, fn dataframe.FilterDataFrameFn) float64 {
	ctx := context.Background()
	filterRes, _ := dataframe.Filter(ctx, df, fn)
	filterDf := filterRes.(*dataframe.DataFrame)
	n := filterDf.NRows()
	if n == 0 {
		return 0.00
	}
	lastRow := filterDf.Row(0, false, dataframe.SeriesName)
	firstRow := filterDf.Row(n-1, false, dataframe.SeriesName)
	lastClose := lastRow["close"].(float64)
	firstPreClose := firstRow["preclose"].(float64)
	return RoundChgRate((lastClose - firstPreClose) / firstPreClose)
}

func chgDays(df *dataframe.DataFrame, days int) float64 {
	n := df.NRows()
	lastRow := df.Row(0, false, dataframe.SeriesName)

	r := days - 1
	if days > n-1 {
		r = n - 1
	}
	firstRow := df.Row(r, false, dataframe.SeriesName)
	lastClose := lastRow["close"].(float64)
	firstPreClose := firstRow["preclose"].(float64)
	return RoundChgRate((lastClose - firstPreClose) / firstPreClose)
}

func avgDays(df *dataframe.DataFrame, days int) float64 {
	idx, _ := df.NameToColumn("close")
	series := df.Series[idx].(*dataframe.SeriesFloat64)
	values := series.Values
	var n int
	if len(values) < days {
		n = len(values)
	} else {
		n = days
	}
	values = values[0:n]
	return Round2(stat.Mean(values, nil))
}

func GetDailyState(code string) (*DailyStat, error) {
	t := store.GetLastTime(code)
	endDay := now.BeginningOfDay()
	var startDay time.Time
	if t.IsZero() {
		logger.SugarLog.Infof("getting history data for %s, it would take some time ...", code)
		startDay = endDay.AddDate(-1, 0, 0)
	} else {
		startDay = t.AddDate(0, 0, 1)

		// skip weekend
		switch startDay.Weekday() {
		case time.Sunday:
			startDay = startDay.AddDate(0, 0, 1)
		case time.Saturday:
			startDay = startDay.AddDate(0, 0, 2)
		}
	}
	startDay = now.With(startDay).BeginningOfDay()

	if endDay.After(startDay) {
		bs := baostock.NewBaoStockInstance()
		err := bs.Login()
		if err != nil {
			return nil, errors.Wrap(err, "failed to login baostock")
		}
		t1 := time.Now()
		rs, err := bs.GetDailyKData(code, startDay, endDay)
		if err != nil {
			return nil, errors.Wrap(err, "get daily state failed")
		}
		records := make([]*store.Record, 0, 1024)
		for rs.Next() {
			seps := rs.GetRowData()
			date, _ := now.Parse(seps[0])
			records = append(records, &store.Record{
				Code: code,
				T:    date,
				Val:  strings.Join(seps, ","),
			})
		}
		store.WriteRecords(records)
		logger.SugarLog.Debugf("[%s] get remote data takes [%v]", code, time.Since(t1))
	}

	df, err := store.GetRecords(code, endDay.AddDate(-1, 0, 0), endDay)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get records from db for [%s]", code)
	}

	if len(df.Series) == 0 {
		return nil, errors.Errorf("empty records from db for [%s]", code)
	}

	name := store.GetName(code, true)
	hq, err := sina.GetLivePrice(code)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get hq from sina")
	}

	lastRecord := df.Row(0, false, dataframe.SeriesName)
	ds := &DailyStat{
		Name:         name,
		ChgToday:     hq.ChgToday,
		Now:          hq.Now,
		Last:         hq.Last,
		ChgLast:      lastRecord["pctChg"].(float64),
		ChgMonth:     chgWithDf(df, thisMonthFilterFn),
		ChgLastMonth: chgWithDf(df, lastMonthFilterFn),
		ChgYear:      chgWithDf(df, thisYearFilterFn),
		Avg20:        avgDays(df, 20),
		Avg60:        avgDays(df, 60),
		Avg200:       avgDays(df, 200),
		Chg20:        chgDays(df, 20),
		Chg60:        chgDays(df, 60),
		Chg90:        chgDays(df, 90),
		Code:         code,
	}
	return ds, nil
}