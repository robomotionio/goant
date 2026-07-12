function check(a,b){if(a!==b)throw a+" !== "+b;} var r=0;try{r=1;}finally{r+=10;}check(r,11);console.log('PASS');
