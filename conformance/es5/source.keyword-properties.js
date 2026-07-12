function check(a,b){if(a!==b)throw a+" !== "+b;} var o={};o['class']=1;o['if']=2;check(o.class,1);check(o.if,2);console.log('PASS');
